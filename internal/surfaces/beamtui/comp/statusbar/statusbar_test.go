package statusbar

import (
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/contenox/beam/internal/surfaces/beamtui/frame"
	"github.com/contenox/beam/internal/surfaces/beamtui/testkit"
	"github.com/contenox/beam/internal/surfaces/beamtui/textwidth"
)

// goldenWidths is the blueprint's resize matrix for the status bar: narrow,
// the two common terminal widths, and wide.
var goldenWidths = []int{40, 60, 80, 120}

// fullHouse populates every segment at once. Its field values are also the
// anchor for TestUnit_DropCascadeOrder's substring markers, so the cascade
// test and the goldens are pinned to the same fixture.
//
// Health is deliberately NOT "working" here: working plus an Activity is the
// one combination that renders no health segment at all (see showHealth), so
// a fixture meant to exercise every segment cannot use it. That combination
// has its own test below.
func fullHouse(ascii bool) State {
	spinner := "◐"
	if ascii {
		spinner = "*"
	}
	return State{
		ASCII:    ascii,
		Session:  "main",
		Messages: 12,
		Model:    "qwen3-coder:30b",
		Provider: "ollama",
		Used:     4200,
		Size:     8000,
		Inbox:    3,
		Missions: 2,
		Health:   HealthReconnecting,
		Activity: "thinking",
		Spinner:  spinner,
	}
}

// TestUnit_RenderGoldens pins the full-house state at the resize matrix,
// unicode and ASCII, including the 120-column "everything fits" case.
func TestUnit_RenderGoldens(t *testing.T) {
	for _, ascii := range []bool{false, true} {
		variant := "unicode"
		if ascii {
			variant = "ascii"
		}
		s := fullHouse(ascii)
		for _, w := range goldenWidths {
			name := fmt.Sprintf("fullhouse_%s_w%d", variant, w)
			t.Run(name, func(t *testing.T) {
				testkit.Golden(t, name, testkit.EncodeLines([]frame.Line{Render(w, s)}))
			})
		}
	}
}

// TestUnit_DropCascadeOrder renders the full-house state at every width
// from comfortably-fits down to nearly nothing, and checks that each
// segment's marker text disappears exactly once, never reappears at a
// smaller width, and does so in the documented drop priority: session,
// activity, missions, gauge, model, health. identity's gutter must remain
// the first character of every rendered line throughout.
func TestUnit_DropCascadeOrder(t *testing.T) {
	markers := []struct {
		name string
		text string
	}{
		{segSession, "main·12"},
		{segActivity, "thinking"},
		{segInbox, "✉ 3"},
		{segMissions, "◇ 2"},
		{segGauge, "4200/8000"},
		{segModel, "qwen3-coder"},
		{segHealth, HealthReconnecting},
	}
	wantOrder := []string{segSession, segActivity, segInbox, segMissions, segGauge, segModel, segHealth}

	s := fullHouse(false)
	present := make(map[string]bool, len(markers))
	for _, m := range markers {
		present[m.name] = true
	}

	var droppedOrder []string
	for width := 100; width >= 5; width-- {
		line := Render(width, s).Text()
		if !strings.HasPrefix(line, "▌") {
			t.Fatalf("width %d: line %q does not start with the identity gutter", width, line)
		}
		for _, m := range markers {
			now := strings.Contains(line, m.text)
			switch {
			case present[m.name] && !now:
				droppedOrder = append(droppedOrder, m.name)
				present[m.name] = false
			case !present[m.name] && now:
				t.Fatalf("width %d: segment %q reappeared after being dropped (line %q)", width, m.name, line)
			}
		}
	}
	if !reflect.DeepEqual(droppedOrder, wantOrder) {
		t.Fatalf("drop order = %v, want %v", droppedOrder, wantOrder)
	}
}

// TestUnit_GaugeThreshold pins the three documented style boundaries: the
// gauge is muted below 75%, warn at/above 75%, and error at/above 90%.
func TestUnit_GaugeThreshold(t *testing.T) {
	cases := []struct {
		used, size int
		want       frame.StyleID
	}{
		{74, 100, frame.StyleMuted},
		{75, 100, frame.StyleWarn},
		{90, 100, frame.StyleError},
	}
	for _, c := range cases {
		s := State{Used: c.used, Size: c.size}
		line := Render(80, s)
		text := fmt.Sprintf("%d/%d (%d%%)", c.used, c.size, c.used*100/c.size)
		style, ok := spanStyle(line, text)
		if !ok {
			t.Fatalf("used=%d size=%d: gauge text %q not found in %q", c.used, c.size, text, line.Text())
		}
		if style != c.want {
			t.Fatalf("used=%d size=%d: gauge style = %q, want %q", c.used, c.size, style, c.want)
		}
	}
}

// TestUnit_HealthStyle pins the error/disconnected-vs-everything-else
// split the blueprint specifies for the health segment.
func TestUnit_HealthStyle(t *testing.T) {
	cases := []struct {
		health string
		want   frame.StyleID
	}{
		{HealthWorking, frame.StyleWarn},
		{HealthReconnecting, frame.StyleWarn},
		{HealthSetupRequired, frame.StyleWarn},
		{HealthError, frame.StyleError},
		{HealthDisconnected, frame.StyleError},
	}
	for _, c := range cases {
		line := Render(80, State{Health: c.health})
		style, ok := spanStyle(line, c.health)
		if !ok {
			t.Fatalf("health %q: span not found in %q", c.health, line.Text())
		}
		if style != c.want {
			t.Fatalf("health %q: style = %q, want %q", c.health, style, c.want)
		}
	}
}

// TestUnit_GaugeHiddenWhenSizeZero guards against ever rendering a
// misleading "0/0" gauge before the first usage_update lands.
func TestUnit_GaugeHiddenWhenSizeZero(t *testing.T) {
	line := Render(80, State{Used: 100, Size: 0}).Text()
	if strings.Contains(line, "100/") || strings.Contains(line, "%") {
		t.Fatalf("gauge rendered with Size==0: %q", line)
	}
	// Even Used==Size==0 (the true zero value) must never print "0/0".
	line = Render(80, State{}).Text()
	if strings.Contains(line, "0/0") {
		t.Fatalf("gauge rendered for zero-value state: %q", line)
	}
}

// TestUnit_InboxBadge guards the operator-inbox badge: the >=1 gate, both glyph
// variants, and the style role.
//
// StyleHITL is asserted rather than left to the golden because it is the badge's
// whole claim. Every other count on this bar reports work going fine; this one
// says a human is needed, and it borrows the approval card's role so a Mono
// terminal — where the glyph is the plain word "in:" — still puts it in the same
// visual register as the thing it is a reminder of.
func TestUnit_InboxBadge(t *testing.T) {
	if zero := Render(80, State{Inbox: 0}).Text(); strings.Contains(zero, "✉") || strings.Contains(zero, "in:") {
		t.Fatalf("inbox badge rendered with Inbox==0: %q", zero)
	}

	one := Render(80, State{Inbox: 1})
	if !strings.Contains(one.Text(), "✉ 1") {
		t.Fatalf("inbox badge missing with Inbox==1: %q", one.Text())
	}
	if style, ok := spanStyle(one, "✉ 1"); !ok || style != frame.StyleHITL {
		t.Fatalf("inbox badge style = %q (found=%v), want %q", style, ok, frame.StyleHITL)
	}

	ascii := Render(80, State{ASCII: true, Inbox: 12})
	if !strings.Contains(ascii.Text(), "in:12") {
		t.Fatalf("ascii inbox badge missing: %q", ascii.Text())
	}
	if strings.Contains(ascii.Text(), "✉") {
		t.Fatalf("ascii line carries the unicode envelope: %q", ascii.Text())
	}

	// The two badges must stay distinguishable in Mono, where neither has a
	// glyph left to tell them apart by.
	both := Render(80, State{ASCII: true, Inbox: 2, Missions: 2}).Text()
	if !strings.Contains(both, "in:2") || !strings.Contains(both, "m:2") {
		t.Fatalf("ascii inbox and missions badges are not both present: %q", both)
	}
}

// TestUnit_MissionsHiddenWhenZero guards the badge's >=1 gate and pins
// both glyph variants.
func TestUnit_MissionsHiddenWhenZero(t *testing.T) {
	zero := Render(80, State{Missions: 0}).Text()
	if strings.Contains(zero, "◇") || strings.Contains(zero, "m:") {
		t.Fatalf("missions badge rendered with Missions==0: %q", zero)
	}
	one := Render(80, State{Missions: 1}).Text()
	if !strings.Contains(one, "◇ 1") {
		t.Fatalf("missions badge missing with Missions==1: %q", one)
	}
	oneASCII := Render(80, State{ASCII: true, Missions: 1}).Text()
	if !strings.Contains(oneASCII, "m:1") {
		t.Fatalf("ascii missions badge missing with Missions==1: %q", oneASCII)
	}
}

// TestUnit_HealthHiddenWhenReady guards the silent-default rule: "ready"
// and the unset zero value both render no health segment at all.
func TestUnit_HealthHiddenWhenReady(t *testing.T) {
	ready := Render(80, State{Health: HealthReady}).Text()
	if strings.Contains(ready, HealthReady) {
		t.Fatalf("health segment rendered for ready state: %q", ready)
	}
	empty := Render(80, State{}).Text()
	if strings.Contains(empty, HealthReady) {
		t.Fatalf("health segment rendered for zero-value state: %q", empty)
	}
	working := Render(80, State{Health: HealthWorking}).Text()
	if !strings.Contains(working, HealthWorking) {
		t.Fatalf("health segment missing for working state: %q", working)
	}
}

// TestUnit_ASCIISeparators pins the ASCII forms of both joined segments: the
// session's message count, which must not read as a suffix of the label it
// follows, and the model's spaced provider separator.
func TestUnit_ASCIISeparators(t *testing.T) {
	s := State{ASCII: true, Session: "beam-20a88ab8", Messages: 3, Model: "gpt", Provider: "openai"}
	line := Render(80, s).Text()
	if !strings.Contains(line, "beam-20a88ab8 (3)") {
		t.Fatalf("ascii session count wrong: %q", line)
	}
	if strings.Contains(line, "beam-20a88ab8-3") {
		t.Fatalf("ascii message count still reads as part of the session id: %q", line)
	}
	if !strings.Contains(line, "gpt - openai") {
		t.Fatalf("ascii model separator wrong: %q", line)
	}
	if strings.Contains(line, "·") {
		t.Fatalf("ascii line contains a unicode middot: %q", line)
	}
}

// TestUnit_UnicodeSeparators is ASCIISeparators' unicode counterpart. The
// model separator is SPACED, matching comp/brand's welcome header, which
// names the same pair a few rows above it.
func TestUnit_UnicodeSeparators(t *testing.T) {
	s := State{Session: "main", Messages: 3, Model: "gpt", Provider: "openai"}
	line := Render(80, s).Text()
	if !strings.Contains(line, "main·3") {
		t.Fatalf("session separator wrong: %q", line)
	}
	if !strings.Contains(line, "gpt · openai") {
		t.Fatalf("model separator wrong: %q", line)
	}
}

// TestUnit_HealthYieldsToActivity is the redundancy rule: while a turn runs
// the app publishes Health=working AND Activity="working" — the same fact
// from two publishers — and the bar said "working" twice for the whole turn.
// Activity wins, because it carries the spinner and the better text; every
// other health state still renders beside an activity, because none of them
// are something the activity already said.
func TestUnit_HealthYieldsToActivity(t *testing.T) {
	both := Render(120, State{Health: HealthWorking, Activity: "working", Spinner: "◐"}).Text()
	if strings.Count(both, "working") != 1 {
		t.Fatalf("want exactly one \"working\" on the bar, got %q", both)
	}

	// No activity: health is the only thing that can say it, so it says it.
	alone := Render(120, State{Health: HealthWorking}).Text()
	if !strings.Contains(alone, "working") {
		t.Fatalf("health dropped with no activity to replace it: %q", alone)
	}

	// Everything else adds information an activity cannot carry.
	for _, h := range []string{HealthError, HealthDisconnected, HealthReconnecting, HealthSetupRequired, "weird"} {
		line := Render(120, State{Health: h, Activity: "working", Spinner: "◐"}).Text()
		if !strings.Contains(line, h) {
			t.Fatalf("health %q dropped beside an activity: %q", h, line)
		}
	}
}

// TestUnit_ExactWidthProperty is the resize contract every frame producer
// in beam must satisfy: the rendered line is always padded/dropped to
// exactly width cells, never more, never less, across the full supported
// width range and several representative states.
func TestUnit_ExactWidthProperty(t *testing.T) {
	states := []State{
		fullHouse(false),
		fullHouse(true),
		{},
		{Health: HealthError},
		{Missions: 3, ASCII: true},
		{Inbox: 9, Missions: 3},
		{Inbox: 9, Missions: 3, ASCII: true},
	}
	for i, s := range states {
		for w := 20; w <= 140; w++ {
			if got := textwidth.Width(Render(w, s).Text()); got != w {
				t.Fatalf("state %d width %d: rendered width %d, want %d", i, w, got, w)
			}
		}
	}
}

// TestUnit_IdentityAlwaysLeftmost checks that whenever the line is
// non-empty, its first character is the identity gutter — identity is
// dropped last (and only ever truncated, never omitted), so it must
// anchor the left edge at every width down to 1.
func TestUnit_IdentityAlwaysLeftmost(t *testing.T) {
	for _, ascii := range []bool{false, true} {
		want := "▌"
		if ascii {
			want = "|"
		}
		s := fullHouse(ascii)
		for w := 1; w <= 140; w++ {
			if line := Render(w, s).Text(); !strings.HasPrefix(line, want) {
				t.Fatalf("ascii=%v width %d: line %q does not start with %q", ascii, w, line, want)
			}
		}
	}
}

// TestUnit_StateStringsAreSanitized: every string on State comes from
// somewhere else — a session name off the wire, a model a provider named,
// activity text composed from events — and this bar is on screen at all
// times, so it is the most valuable row in the terminal to be able to write
// to.
//
// The tab and the newline are not merely hygiene here. The bar pads itself to
// EXACTLY width by counting cells on plain text, so a tab breaks the
// arithmetic; and a newline inside a span violates frame.Line's one-row
// contract, which would have the engine scroll the screen from the status bar
// on every repaint.
func TestUnit_StateStringsAreSanitized(t *testing.T) {
	probes := []struct {
		name  string
		state State
	}{
		{"session tab", State{Session: "my\tsession"}},
		{"session newline", State{Session: "my\nsession"}},
		{"session escape", State{Session: "my\x1b[2Jsession"}},
		{"model osc", State{Model: "gpt\x1b]0;pwned\x07", Provider: "prov\x1bider"}},
		{"health control", State{Health: "err\x07or"}},
		{"activity bidi", State{Activity: "think‮gni", Spinner: "\x1b[31m*"}},
		{"everything", State{
			Session: "s\x1b[2J\t\n", Messages: 3,
			Model: "m\x7f", Provider: "p\t", Health: "h\n",
			Activity: "a\x1b]0;x\x07", Spinner: "\t",
			Used: 1, Size: 2, Missions: 1, Inbox: 1,
		}},
	}

	for _, p := range probes {
		t.Run(p.name, func(t *testing.T) {
			for _, ascii := range []bool{false, true} {
				st := p.state
				st.ASCII = ascii
				for _, w := range []int{20, 40, 80, 120} {
					line := Render(w, st)
					for _, sp := range line {
						for _, r := range sp.Text {
							if r < 0x20 || r == 0x7f {
								t.Fatalf("width %d: span %q carries %U", w, sp.Text, r)
							}
							if (r >= 0x202a && r <= 0x202e) || (r >= 0x2066 && r <= 0x2069) {
								t.Fatalf("width %d: span %q carries bidi control %U", w, sp.Text, r)
							}
						}
					}
					// The exact-width contract has to survive the cleaning:
					// the cells a stripped control used to occupy are the
					// padding's problem, not the caller's.
					if got := textwidth.Width(line.Text()); got != w {
						t.Fatalf("width %d: rendered %d cells", w, got)
					}
				}
			}
		})
	}

	// The reviewer's probes, spelled out: neither shape may reach a span.
	if got := Render(40, State{Session: "my\tsession"}).Text(); strings.ContainsRune(got, '\t') {
		t.Fatalf("tab survived into %q", got)
	}
	if got := Render(40, State{Session: "my\nsession"}).Text(); strings.ContainsRune(got, '\n') {
		t.Fatalf("newline survived into %q", got)
	}
}

// TestUnit_UsesOnlyClosedStyleIDs enforces frame's closed StyleID set: no
// span may carry a role the style package's table doesn't know.
func TestUnit_UsesOnlyClosedStyleIDs(t *testing.T) {
	known := map[frame.StyleID]bool{}
	for _, id := range frame.All() {
		known[id] = true
	}
	states := []State{fullHouse(false), fullHouse(true), {}, {Health: HealthError, Missions: 5, Inbox: 5}}
	for _, s := range states {
		for _, w := range []int{1, 5, 20, 40, 60, 80, 120} {
			for _, sp := range Render(w, s) {
				if !known[sp.Style] {
					t.Fatalf("width %d: span %q uses unknown StyleID %q", w, sp.Text, sp.Style)
				}
			}
		}
	}
}

func spanStyle(l frame.Line, text string) (frame.StyleID, bool) {
	for _, sp := range l {
		if sp.Text == text {
			return sp.Style, true
		}
	}
	return "", false
}

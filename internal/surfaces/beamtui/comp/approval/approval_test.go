package approval

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/contenox/beam/internal/services/approvalflow"
	"github.com/contenox/beam/internal/surfaces/beamtui/enginebridge"
	"github.com/contenox/beam/internal/surfaces/beamtui/frame"
	"github.com/contenox/beam/internal/surfaces/beamtui/testkit"
	"github.com/contenox/beam/internal/surfaces/beamtui/textwidth"
	libacp "github.com/contenox/beam/libacp"
)

// goldenWidths is the blueprint's resize matrix: narrow, default terminal,
// wide.
var goldenWidths = []int{60, 80, 120}

const sampleDiff = `--- a/internal/app.go
+++ b/internal/app.go
@@ -12,3 +12,3 @@
-    return errors.New("todo")
+    return run(ctx, cfg)`

// sampleEvent is a realistic gated write: three arguments (one of them a
// body far past the display cap), a named policy rule, and a short diff.
func sampleEvent(resolve func(bool)) enginebridge.PermissionRequested {
	args := map[string]any{
		"path":    "/repo/internal/app.go",
		"content": strings.Repeat("a line of replacement content\n", 40),
		"create":  true,
	}
	raw, err := json.Marshal(args)
	if err != nil {
		panic(err)
	}
	return enginebridge.PermissionRequested{
		SessionID:  libacp.SessionID("sess-7f3a"),
		ToolCallID: "local_fs.write_file",
		Title:      "local_fs.write_file: /repo/internal/app.go",
		Kind:       libacp.ToolKindEdit,
		Status:     libacp.ToolCallStatusPending,
		Meta: approvalflow.Meta{
			ToolsName:  "local_fs",
			ToolName:   "write_file",
			PolicyName: "guarded",
			PolicyPath: "rules[3].local_fs.write_file",
			Diff:       sampleDiff,
		},
		RawInput: raw,
		Options: []libacp.PermissionOption{
			{OptionID: approvalflow.OptionAllow, Name: "Allow", Kind: libacp.PermissionAllowOnce},
			{OptionID: approvalflow.OptionDeny, Name: "Deny", Kind: libacp.PermissionRejectOnce},
		},
		Resolve: resolve,
	}
}

func texts(lines []frame.Line) []string {
	out := make([]string, 0, len(lines))
	for _, l := range lines {
		out = append(out, l.Text())
	}
	return out
}

// TestUnit_CardGoldens pins the whole card in every state and both glyph
// variants. The ORDER is the contract inherited from hitl_tty.go: identity,
// sorted args, policy, diff, decision.
func TestUnit_CardGoldens(t *testing.T) {
	states := []struct {
		name  string
		apply func(*Card)
	}{
		{"pending", func(*Card) {}},
		{"allowed", func(c *Card) { c.Resolve(true) }},
		{"denied", func(c *Card) { c.Resolve(false) }},
		{"cancelled", func(c *Card) { c.MarkCancelled() }},
	}
	for _, s := range states {
		for _, ascii := range []bool{false, true} {
			for _, w := range goldenWidths {
				label := "unicode"
				if ascii {
					label = "ascii"
				}
				name := fmt.Sprintf("card_%s_%s_w%d", s.name, label, w)
				t.Run(name, func(t *testing.T) {
					c := New(sampleEvent(func(bool) {}))
					s.apply(c)
					testkit.Golden(t, name, testkit.EncodeLines(c.Render(w, ascii, "⠋")))
				})
			}
		}
	}
}

// TestUnit_ArgsAreSortedAndSummarised: a human pattern-matching under
// fatigue gets the same layout every time, and a 1 KB body reads as one
// line with its true size visible rather than scrolling the diff away.
func TestUnit_ArgsAreSortedAndSummarised(t *testing.T) {
	c := New(sampleEvent(nil))
	lines := texts(c.Render(200, false, ""))

	if want := "◆ approval required"; lines[0] != want {
		t.Fatalf("header = %q, want %q", lines[0], want)
	}
	if want := "tool  local_fs.write_file"; lines[1] != want {
		t.Fatalf("tool line = %q, want %q", lines[1], want)
	}

	args := lines[2:5]
	if !strings.HasPrefix(args[0], "  content = ") ||
		!strings.HasPrefix(args[1], "  create = ") ||
		!strings.HasPrefix(args[2], "  path = ") {
		t.Fatalf("args are not in sorted order: %q", args)
	}
	// The path comes back through approvalflow's own summariser, verbatim.
	if want := "  path = /repo/internal/app.go"; args[2] != want {
		t.Fatalf("path arg = %q, want %q", args[2], want)
	}
	if args[1] != "  create = true" {
		t.Fatalf("bool arg = %q", args[1])
	}
	// The body is elided WITH its size — hitl_tty.go's visible-elision rule.
	if !strings.Contains(args[0], "bytes") || !strings.Contains(args[0], "lines") {
		t.Fatalf("long arg lacks a visible elision marker: %q", args[0])
	}
	if strings.Contains(args[0], "\n") {
		t.Fatalf("long arg leaked a newline into a span: %q", args[0])
	}

	if want := "policy guarded · rule rules[3].local_fs.write_file"; lines[5] != want {
		t.Fatalf("policy line = %q, want %q", lines[5], want)
	}
}

// TestUnit_DiffIsLast is the ordering rule that motivated the CLI layout:
// the change under review sits immediately above the decision.
func TestUnit_DiffIsLast(t *testing.T) {
	c := New(sampleEvent(nil))
	lines := texts(c.Render(200, false, ""))

	last := len(lines) - 1
	if !strings.Contains(lines[last], "allow") {
		t.Fatalf("last line = %q, want the decision line", lines[last])
	}
	diffBody := lines[last-5 : last]
	if !equal(diffBody, strings.Split(sampleDiff, "\n")) {
		t.Fatalf("diff body = %q, want it verbatim directly above the decision", diffBody)
	}

	// And its lines are classified by their own first character.
	rendered := c.Render(200, false, "")
	styles := map[string]frame.StyleID{}
	for _, l := range rendered[len(rendered)-6 : len(rendered)-1] {
		styles[l.Text()] = l[0].Style
	}
	if got := styles[`+    return run(ctx, cfg)`]; got != frame.StyleDone {
		t.Fatalf("added line style = %q, want done", got)
	}
	if got := styles[`-    return errors.New("todo")`]; got != frame.StyleError {
		t.Fatalf("removed line style = %q, want error", got)
	}
	if got := styles["@@ -12,3 +12,3 @@"]; got != frame.StyleMuted {
		t.Fatalf("hunk header style = %q, want muted", got)
	}
}

// TestUnit_DiffTruncationWarnsWithExactCounts: the cap is hitl_tty.go's
// 120, and the notice states the consequence of approving unseen changes.
func TestUnit_DiffTruncationWarnsWithExactCounts(t *testing.T) {
	body := make([]string, 200)
	for i := range body {
		body[i] = fmt.Sprintf("+line %d", i)
	}
	ev := sampleEvent(nil)
	ev.Meta.Diff = strings.Join(body, "\n")
	c := New(ev)

	lines := texts(c.Render(120, false, ""))
	shown := 0
	for _, l := range lines {
		if strings.HasPrefix(l, "+line ") {
			shown++
		}
	}
	if shown != maxDiffLines {
		t.Fatalf("rendered %d diff lines, want %d", shown, maxDiffLines)
	}
	// The cut is the FIRST 120 lines: body, then warning, then decision.
	if got := lines[len(lines)-3]; got != "+line 119" {
		t.Fatalf("last shown diff line = %q, want +line 119", got)
	}

	warning := lines[len(lines)-2]
	for _, want := range []string{"⚠", "diff truncated", "showing 120 of 200", "approving accepts changes you have not seen"} {
		if !strings.Contains(warning, want) {
			t.Fatalf("warning = %q, missing %q", warning, want)
		}
	}
	if got := c.Render(120, false, "")[len(lines)-2][0].Style; got != frame.StyleWarn {
		t.Fatalf("warning style = %q, want warn", got)
	}

	// The warning is the last thing before the decision line.
	if !strings.Contains(lines[len(lines)-1], "allow") {
		t.Fatalf("line after the warning = %q, want the decision line", lines[len(lines)-1])
	}

	// A diff exactly at the cap does not warn.
	ev.Meta.Diff = strings.Join(body[:maxDiffLines], "\n")
	for _, l := range texts(New(ev).Render(120, false, "")) {
		if strings.Contains(l, "diff truncated") {
			t.Fatal("a diff exactly at the cap warned")
		}
	}
}

// TestUnit_TruncationWarningSurvivesNarrowWidths: the one line whose whole
// sentence is load-bearing wraps instead of being elided.
func TestUnit_TruncationWarningSurvivesNarrowWidths(t *testing.T) {
	body := make([]string, 200)
	for i := range body {
		body[i] = fmt.Sprintf("+line %d", i)
	}
	ev := sampleEvent(nil)
	ev.Meta.Diff = strings.Join(body, "\n")

	for _, ascii := range []bool{false, true} {
		for _, w := range []int{40, 60, 80, 120} {
			var warn strings.Builder
			for _, l := range New(ev).Render(w, ascii, "") {
				if l[0].Style == frame.StyleWarn {
					warn.WriteString(l.Text())
				}
			}
			// The warning wraps, so it is reassembled by concatenation: no
			// character of it may be dropped at any width.
			if got := warn.String(); got != truncationWarning(200, ascii) {
				t.Fatalf("ascii=%v width %d: warning reassembles to %q, want %q",
					ascii, w, got, truncationWarning(200, ascii))
			}
		}
	}
}

// TestUnit_DiffLinesAreUnwrapped is the copy-cleanliness property: a diff
// body line is emitted verbatim as ONE span even when it is wider than the
// terminal, so selecting it yields the real line — never a wrapped or
// elided approximation.
func TestUnit_DiffLinesAreUnwrapped(t *testing.T) {
	long := "+" + strings.Repeat("x", 300)
	ev := sampleEvent(nil)
	ev.Meta.Diff = "@@ -1 +1 @@\n" + long

	for _, w := range []int{20, 40, 80} {
		lines := New(ev).Render(w, false, "")
		found := false
		for _, l := range lines {
			if l.Text() == long {
				found = true
				if len(l) != 1 {
					t.Fatalf("width %d: diff line split into %d spans", w, len(l))
				}
			}
			// Chrome may be clamped; a diff body line never is.
			if isDiffBody(l.Text()) && strings.Contains(l.Text(), "…") {
				t.Fatalf("width %d: elision marker inside a diff line: %q", w, l.Text())
			}
		}
		if !found {
			t.Fatalf("width %d: the 300-cell diff line was not emitted verbatim", w)
		}
	}
}

// TestUnit_DiffLinesAreSanitized is the security half of the diff's verbatim
// rule. A diff is the most attacker-controlled string on this card — it comes
// out of a repository and is shown to a human as the exact change they are
// authorising — so "verbatim" cannot mean "raw bytes to the terminal": a CSI
// erases the lines above it, and a bidi override displays a line as the
// reverse of what it applies. Neither is allowed to reach a span; the diff's
// exemption is from WRAPPING and ELISION, not from sanitizing.
func TestUnit_DiffLinesAreSanitized(t *testing.T) {
	cases := []struct {
		name string
		line string
		want string
	}{
		{"clear screen", "+evil\x1b[2Jclear", "+evilclear"},
		{"bidi override", "+‮drawkcab", "+drawkcab"},
		{"osc title", "-\x1b]0;pwned\x07gone", "-gone"},
		// Indentation is part of what is being reviewed: a tab expands to its
		// column stop, it does not fold to one space and it does not survive
		// into a span as a tab.
		{"tab indent", "+\tfmt.Println", "+       fmt.Println"},
		{"two tab levels", "+\t\tx := 1", "+               x := 1"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ev := sampleEvent(nil)
			ev.Meta.Diff = "@@ -1 +1 @@\n" + c.line
			lines := New(ev).Render(200, false, "")

			found := false
			for _, l := range lines {
				for _, sp := range l {
					for _, r := range sp.Text {
						if r < 0x20 || r == 0x7f {
							t.Fatalf("span %q carries %U", sp.Text, r)
						}
						if (r >= 0x202a && r <= 0x202e) || (r >= 0x2066 && r <= 0x2069) {
							t.Fatalf("span %q carries bidi control %U", sp.Text, r)
						}
					}
				}
				if l.Text() == c.want {
					found = true
					if len(l) != 1 {
						t.Fatalf("diff line split into %d spans", len(l))
					}
				}
			}
			if !found {
				var got []string
				for _, l := range lines {
					got = append(got, l.Text())
				}
				t.Fatalf("no line rendered as %q; card was %q", c.want, got)
			}
		})
	}
}

// TestUnit_TabIndentSurvivesAtEveryWidth: expansion happens once, at ingest,
// so a diff line's indentation is the same string whatever the terminal is —
// and it is still one unwrapped span.
func TestUnit_TabIndentSurvivesAtEveryWidth(t *testing.T) {
	const want = "+       fmt.Println(\"a line long enough that a narrow card cannot hold it at all\")"
	ev := sampleEvent(nil)
	ev.Meta.Diff = "@@ -1 +1 @@\n+\tfmt.Println(\"a line long enough that a narrow card cannot hold it at all\")"

	for _, w := range []int{20, 40, 80, 200} {
		found := false
		for _, l := range New(ev).Render(w, false, "") {
			if l.Text() == want {
				found = true
			}
		}
		if !found {
			t.Fatalf("width %d: the tab-indented diff line was not emitted as %q", w, want)
		}
	}
}

// TestUnit_CardChromeIsSanitized: every peer-supplied string on the card —
// not just the diff — is somebody else's, and the card names what the
// operator is about to authorise. None of it may rewrite the card around
// itself.
func TestUnit_CardChromeIsSanitized(t *testing.T) {
	const evil = "a\x1b[2Jb\x1b]0;t\x07c\td\x7f‮e"

	ev := sampleEvent(nil)
	ev.Title = evil
	ev.Meta.ToolsName = evil
	ev.Meta.ToolName = evil
	ev.Meta.PolicyName = evil
	ev.Meta.PolicyPath = evil
	raw, err := json.Marshal(map[string]any{"path": evil, "body": strings.Repeat(evil, 30)})
	if err != nil {
		t.Fatal(err)
	}
	ev.RawInput = raw

	for _, ascii := range []bool{false, true} {
		for _, l := range New(ev).Render(120, ascii, "") {
			for _, sp := range l {
				for _, r := range sp.Text {
					if r < 0x20 || r == 0x7f {
						t.Fatalf("ascii=%v: span %q carries %U", ascii, sp.Text, r)
					}
					if (r >= 0x202a && r <= 0x202e) || (r >= 0x2066 && r <= 0x2069) {
						t.Fatalf("ascii=%v: span %q carries bidi control %U", ascii, sp.Text, r)
					}
				}
			}
		}
	}

	// A non-object payload takes the other branch and must fare the same.
	ev.RawInput = json.RawMessage(`"` + `[2Jraw` + `"`)
	for _, l := range New(ev).Render(120, false, "") {
		if strings.ContainsRune(l.Text(), 0x1b) {
			t.Fatalf("raw-input branch leaked an escape: %q", l.Text())
		}
	}
}

// TestUnit_ResolveCallsTheBridgeExactlyOnce: a doubled keystroke, or a
// resolve after a cancel, must never answer twice.
func TestUnit_ResolveCallsTheBridgeExactlyOnce(t *testing.T) {
	var calls []bool
	c := New(sampleEvent(func(allow bool) { calls = append(calls, allow) }))

	c.Resolve(true)
	c.Resolve(true)
	c.Resolve(false)
	c.MarkCancelled()

	if len(calls) != 1 || calls[0] != true {
		t.Fatalf("underlying Resolve calls = %v, want exactly [true]", calls)
	}
	if c.State() != StateResolved || !c.Allowed() {
		t.Fatalf("state = %v allowed = %v, want resolved/allowed", c.State(), c.Allowed())
	}
	if got := texts(c.Render(80, false, "⠋")); got[len(got)-1] != "✓ allowed" {
		t.Fatalf("footer = %q", got[len(got)-1])
	}

	// A denial is the same contract in the other direction.
	calls = nil
	d := New(sampleEvent(func(allow bool) { calls = append(calls, allow) }))
	d.Resolve(false)
	d.Resolve(true)
	if len(calls) != 1 || calls[0] != false {
		t.Fatalf("deny path calls = %v, want exactly [false]", calls)
	}
	if got := texts(d.Render(80, false, "⠋")); got[len(got)-1] != "✗ denied" {
		t.Fatalf("footer = %q", got[len(got)-1])
	}

	// A card whose event carries no Resolve func must not panic.
	New(sampleEvent(nil)).Resolve(true)
}

// TestUnit_CancelledCardStopsWaiting is item 10: a cancelled turn flips its
// pending card rather than leaving it spinning — and does NOT put an
// answer on the wire the operator never gave.
func TestUnit_CancelledCardStopsWaiting(t *testing.T) {
	var calls int
	c := New(sampleEvent(func(bool) { calls++ }))

	c.MarkCancelled()
	if calls != 0 {
		t.Fatalf("MarkCancelled called the bridge %d times, want 0", calls)
	}
	if c.State() != StateCancelled {
		t.Fatalf("state = %v", c.State())
	}
	lines := texts(c.Render(80, false, "⠋"))
	if got := lines[len(lines)-1]; got != "— cancelled" {
		t.Fatalf("footer = %q", got)
	}
	if strings.Contains(strings.Join(lines, "\n"), "⠋") {
		t.Fatal("a cancelled card is still showing a spinner")
	}

	// Cancelling a decided card leaves the decision standing.
	d := New(sampleEvent(func(bool) {}))
	d.Resolve(true)
	d.MarkCancelled()
	if d.State() != StateResolved || !d.Allowed() {
		t.Fatalf("cancel overwrote a decision: %v/%v", d.State(), d.Allowed())
	}
}

// TestUnit_PendingFooterCarriesTheSpinner: the pending line is the only
// animated part of the card, and it degrades to keys alone without one.
func TestUnit_PendingFooterCarriesTheSpinner(t *testing.T) {
	c := New(sampleEvent(nil))

	withSpinner := texts(c.Render(80, false, "⠙"))
	if got := withSpinner[len(withSpinner)-1]; got != "⠙ y allow · n deny · Esc cancels turn" {
		t.Fatalf("footer = %q", got)
	}
	without := texts(c.Render(80, false, ""))
	if got := without[len(without)-1]; got != "y allow · n deny · Esc cancels turn" {
		t.Fatalf("spinnerless footer = %q", got)
	}
	ascii := texts(c.Render(80, true, ""))
	if got := ascii[len(ascii)-1]; got != "y allow - n deny - Esc cancels turn" {
		t.Fatalf("ascii footer = %q", got)
	}
}

// TestUnit_NoDiffFallsBackToANewContentSummary: never a blank diff section,
// and never a dump of content that was never diffed (item 1).
func TestUnit_NoDiffFallsBackToANewContentSummary(t *testing.T) {
	ev := sampleEvent(nil)
	ev.Meta.Diff = ""
	ev.Meta.DiffNew = strings.Repeat("fresh line\n", 12)
	lines := texts(New(ev).Render(80, false, ""))

	if got := lines[len(lines)-2]; got != "new content (12 lines)" {
		t.Fatalf("summary line = %q", got)
	}
	if strings.Contains(strings.Join(lines, "\n"), "fresh line") {
		t.Fatal("the un-diffed body was dumped into the card")
	}

	// Neither diff nor new content: args-only, no empty section.
	ev.Meta.DiffNew = ""
	lines = texts(New(ev).Render(80, false, ""))
	if got := lines[len(lines)-2]; !strings.HasPrefix(got, "policy ") {
		t.Fatalf("line above the decision = %q, want the policy line", got)
	}
}

// TestUnit_RawInputShapes: a non-object payload is shown rather than
// dropped, and an absent one produces no argument rows at all.
func TestUnit_RawInputShapes(t *testing.T) {
	ev := sampleEvent(nil)
	ev.Meta.Diff = ""
	ev.RawInput = json.RawMessage(`["ls","-la"]`)
	lines := texts(New(ev).Render(80, false, ""))
	if got := lines[2]; got != `  ["ls","-la"]` {
		t.Fatalf("non-object raw input rendered as %q", got)
	}

	ev.RawInput = nil
	lines = texts(New(ev).Render(80, false, ""))
	if len(lines) != 4 {
		t.Fatalf("argument-less card = %q, want header/tool/policy/decision", lines)
	}
}

// TestUnit_ToolIdentityFallsBackToTitle: a peer that sent no _meta still
// gets a card that names what it is asking about.
func TestUnit_ToolIdentityFallsBackToTitle(t *testing.T) {
	ev := sampleEvent(nil)
	ev.Meta = approvalflow.Meta{}
	if got := texts(New(ev).Render(120, false, ""))[1]; got != "tool  local_fs.write_file: /repo/internal/app.go" {
		t.Fatalf("tool line = %q, want the event Title", got)
	}

	ev.Title = ""
	if got := texts(New(ev).Render(120, false, ""))[1]; got != "tool  unknown tool" {
		t.Fatalf("tool line = %q", got)
	}

	ev.Meta.ToolName = "write_file"
	if got := texts(New(ev).Render(120, false, ""))[1]; got != "tool  write_file" {
		t.Fatalf("tool line = %q", got)
	}
}

// TestUnit_RenderNeverExceedsWidth is the resize property for everything
// except diff bodies, whose exemption is deliberate and tested above.
func TestUnit_RenderNeverExceedsWidth(t *testing.T) {
	ev := sampleEvent(nil)
	ev.Meta.Diff = ""
	ev.Meta.DiffNew = strings.Repeat("x\n", 5)
	ev.Meta.PolicyPath = strings.Repeat("rules[3].local_fs.write_file/", 6)
	args := map[string]any{
		"path":    strings.Repeat("/very/long/path/segment", 12),
		"content": strings.Repeat("wide 東京 content ", 40),
		"nested":  map[string]any{"a": 1, "b": []any{"c", "d"}},
	}
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	ev.RawInput = raw

	for _, ascii := range []bool{false, true} {
		for _, spinner := range []string{"", "⠋"} {
			c := New(ev)
			for w := 4; w <= 140; w++ {
				for i, l := range c.Render(w, ascii, spinner) {
					if got := textwidth.Width(l.Text()); got > w {
						t.Fatalf("ascii=%v width %d line %d: %d cells (%q)", ascii, w, i, got, l.Text())
					}
				}
			}
		}
	}
	if got := New(ev).Render(0, false, ""); got != nil {
		t.Fatal("zero width rendered lines")
	}
}

// isDiffBody reports whether a rendered line came out of the diff, by the
// same first-character rule the renderer classifies with.
func isDiffBody(s string) bool {
	return strings.HasPrefix(s, "+") || strings.HasPrefix(s, "-") || strings.HasPrefix(s, "@@")
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

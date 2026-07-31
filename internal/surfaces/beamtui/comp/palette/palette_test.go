package palette

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/contenox/contenox/internal/surfaces/beamtui/frame"
	"github.com/contenox/contenox/internal/surfaces/beamtui/testkit"
	"github.com/contenox/contenox/internal/surfaces/beamtui/textwidth"
	libacp "github.com/contenox/libacp"
)

// goldenWidths is the resize matrix: narrow, default terminal, wide.
var goldenWidths = []int{60, 80, 120}

// remoteSet stands in for one available_commands_update; help and mission
// exist to test shadowing and verbatim hint reproduction.
func remoteSet() []libacp.AvailableCommand {
	return []libacp.AvailableCommand{
		{Name: "model", Description: "Switch the session model"},
		{Name: "mission", Description: "Dispatch a detached mission", Input: &libacp.AvailableCommandInput{Hint: "<agent> <objective>"}},
		{Name: "help", Description: "List the available commands."},
		{Name: "compact", Description: "Compact the session history"},
		{Name: "doctor", Description: "Report runtime health"},
		{Name: "policy", Description: "Select the HITL policy preset", Input: &libacp.AvailableCommandInput{Hint: "<preset>"}},
	}
}

// appPalette wires a palette the way the app does: remote set plus its two locals.
func appPalette(t *testing.T) *Palette {
	t.Helper()
	p := New()
	p.SetRemote(remoteSet())
	p.MustRegisterLocal("/quit", "Leave beam", "")
	p.MustRegisterLocal("/help", "Show beam's keys and commands", "")
	return p
}

func names(ents []Entry) []string {
	out := make([]string, 0, len(ents))
	for _, e := range ents {
		out = append(out, e.Name)
	}
	return out
}

// TestUnit_FreshPaletteKnowsNothing: a palette told nothing offers nothing.
func TestUnit_FreshPaletteKnowsNothing(t *testing.T) {
	p := New()
	p.Open("")
	if got := p.Filtered(); len(got) != 0 {
		t.Fatalf("fresh palette offers %v, want nothing", names(got))
	}
	if _, ok := p.Selected(); ok {
		t.Fatal("fresh palette has a selection")
	}
}

// TestUnit_SetRemoteReplacesTheSet: SetRemote replaces the set; it never merges.
func TestUnit_SetRemoteReplacesTheSet(t *testing.T) {
	p := New()
	p.SetRemote(remoteSet())
	p.Open("")
	if len(p.Filtered()) != len(remoteSet()) {
		t.Fatalf("got %v", names(p.Filtered()))
	}

	p.SetRemote([]libacp.AvailableCommand{{Name: "ask", Description: "Ask the delegate"}})
	got := names(p.Filtered())
	if len(got) != 1 || got[0] != "ask" {
		t.Fatalf("after replacement got %v, want [ask]", got)
	}
}

// TestUnit_SetRemoteKeepsTheSelectionOnItsCommand: the selection follows its
// command by name across a SetRemote update, not the index.
func TestUnit_SetRemoteKeepsTheSelectionOnItsCommand(t *testing.T) {
	p := appPalette(t)
	p.Open("")
	p.Move(3) // compact, doctor, help, [mission]
	if e, _ := p.Selected(); e.Name != "mission" {
		t.Fatalf("fixture drifted: selection is %q, want mission", e.Name)
	}

	// A command sorting in ahead of it must not drag the selection along.
	p.SetRemote(append(remoteSet(), libacp.AvailableCommand{Name: "alpha", Description: "sorts first"}))
	if e, _ := p.Selected(); e.Name != "mission" {
		t.Fatalf("after an insertion the selection is %q, want it still on mission", e.Name)
	}

	// When the selected command is gone there is nothing to follow, so the
	// index clamps into the new set rather than pointing past its end. The
	// locals survive a remote replacement, so the set here is help/only/quit
	// and the old index 3 clamps onto the last of them.
	p.SetRemote([]libacp.AvailableCommand{{Name: "only", Description: "the last one standing"}})
	e, ok := p.Selected()
	if !ok {
		t.Fatal("Selected reported not-ok with commands present")
	}
	if got := names(p.Filtered()); !equal(got, []string{"help", "only", "quit"}) {
		t.Fatalf("set after replacement = %v", got)
	}
	if e.Name != "quit" {
		t.Fatalf("selection = %q, want the clamp onto the last remaining command", e.Name)
	}

	// With no locals to fall back on, an emptied set leaves nothing selected
	// and nothing Enter could dispatch.
	bare := New()
	bare.SetRemote(remoteSet())
	bare.Open("")
	bare.Move(2)
	bare.SetRemote(nil)
	if _, ok := bare.Selected(); ok {
		t.Fatal("an emptied palette still reports a selection")
	}
}

// TestUnit_PaletteEntriesAreSanitized: peer-supplied names, descriptions and
// hints never carry control or bidi characters into the render.
func TestUnit_PaletteEntriesAreSanitized(t *testing.T) {
	const evil = "ev\x1b[2Jil\x1b]0;t\x07\tname\x7f‮x"

	p := New()
	p.SetRemote([]libacp.AvailableCommand{{
		Name:        evil,
		Description: evil,
		Input:       &libacp.AvailableCommandInput{Hint: evil},
	}})
	if err := p.RegisterLocal("local", evil, evil); err != nil {
		t.Fatalf("RegisterLocal: %v", err)
	}
	p.Open("")

	for _, ascii := range []bool{false, true} {
		for _, l := range p.Render(200, 8, ascii) {
			for _, s := range l {
				for _, r := range s.Text {
					if r < 0x20 || r == 0x7f {
						t.Fatalf("span %q carries %U", s.Text, r)
					}
					if (r >= 0x202a && r <= 0x202e) || (r >= 0x2066 && r <= 0x2069) {
						t.Fatalf("span %q carries bidi control %U", s.Text, r)
					}
				}
			}
		}
	}
	// The hint reaches the composer's argument line, so it is gated too.
	if hint, ok := p.ArgHint("/local "); ok && strings.ContainsRune(hint, 0x1b) {
		t.Fatalf("ArgHint leaked an escape: %q", hint)
	}
	for _, e := range p.Filtered() {
		if strings.ContainsAny(e.Name+e.Description+e.Hint, "\x1b\t\x7f") {
			t.Fatalf("entry %+v carries a control character", e)
		}
	}
}

// TestUnit_FilterIsAlphabeticalPrefixAndShadowed: bare `/` lists everything
// alphabetically, a prefix narrows it, and a local shadows its remote twin.
func TestUnit_FilterIsAlphabeticalPrefixAndShadowed(t *testing.T) {
	p := appPalette(t)

	p.Open("")
	want := []string{"compact", "doctor", "help", "mission", "model", "policy", "quit"}
	if got := names(p.Filtered()); !equal(got, want) {
		t.Fatalf("bare / = %v, want %v", got, want)
	}

	// help appears once, and it is the local.
	for _, e := range p.Filtered() {
		if e.Name != "help" {
			continue
		}
		if !e.Local {
			t.Fatal("remote help shadowed the local one")
		}
		if e.Description != "Show beam's keys and commands" {
			t.Fatalf("help description = %q, want the local's", e.Description)
		}
	}

	p.SetQuery("m")
	if got := names(p.Filtered()); !equal(got, []string{"mission", "model"}) {
		t.Fatalf("/m = %v", got)
	}
	p.SetQuery("mo")
	if got := names(p.Filtered()); !equal(got, []string{"model"}) {
		t.Fatalf("/mo = %v", got)
	}
	p.SetQuery("zzz")
	if got := names(p.Filtered()); len(got) != 0 {
		t.Fatalf("/zzz = %v, want nothing", got)
	}
}

// TestUnit_FilterIsCaseInsensitive: the prefix filter ignores case.
func TestUnit_FilterIsCaseInsensitive(t *testing.T) {
	p := appPalette(t)
	p.SetRemote(append(remoteSet(), libacp.AvailableCommand{Name: "Review", Description: "Review the diff"}))

	for _, q := range []string{"MI", "mi", "Mi", "mI"} {
		p.Open(q)
		if got := names(p.Filtered()); !equal(got, []string{"mission"}) {
			t.Fatalf("query %q = %v, want [mission]", q, got)
		}
	}
	p.Open("rev")
	if got := names(p.Filtered()); !equal(got, []string{"Review"}) {
		t.Fatalf("lowercase query against a capitalised name = %v", got)
	}
}

// TestUnit_QueryNormalisation: a raw buffer or a bare command token filter identically.
func TestUnit_QueryNormalisation(t *testing.T) {
	p := appPalette(t)
	for _, q := range []string{"mi", "/mi", " /mi ", "/mission arg", "mission arg"} {
		p.Open(q)
		if got := names(p.Filtered()); len(got) == 0 || got[0] != "mission" {
			t.Fatalf("query %q = %v, want mission first", q, got)
		}
	}
}

// TestUnit_RegisterLocalCollisionFailsFast: a duplicate local name errors
// (or panics via MustRegisterLocal) instead of silently overwriting.
func TestUnit_RegisterLocalCollisionFailsFast(t *testing.T) {
	p := New()
	if err := p.RegisterLocal("/sessions", "Switch session", ""); err != nil {
		t.Fatalf("first registration: %v", err)
	}
	err := p.RegisterLocal("sessions", "Browse missions", "")
	if !errors.Is(err, ErrDuplicateLocal) {
		t.Fatalf("second registration err = %v, want ErrDuplicateLocal", err)
	}
	// The winner is the first registration, unchanged.
	e, ok := p.Lookup("/sessions")
	if !ok || e.Description != "Switch session" {
		t.Fatalf("lookup after collision = %+v, %v", e, ok)
	}

	for _, bad := range []string{"", "  ", "/", "two words"} {
		if err := p.RegisterLocal(bad, "x", ""); !errors.Is(err, ErrInvalidName) {
			t.Fatalf("RegisterLocal(%q) err = %v, want ErrInvalidName", bad, err)
		}
	}

	defer func() {
		if recover() == nil {
			t.Fatal("MustRegisterLocal did not panic on a duplicate")
		}
	}()
	p.MustRegisterLocal("sessions", "Browse missions", "")
}

// TestUnit_SelectionAndComplete: Move clamps at both ends, and CompleteText
// inserts the name plus one space without submitting.
func TestUnit_SelectionAndComplete(t *testing.T) {
	p := appPalette(t)
	p.Open("m")

	e, ok := p.Selected()
	if !ok || e.Name != "mission" {
		t.Fatalf("initial selection = %+v, %v", e, ok)
	}
	text, ok := p.CompleteText()
	if !ok || text != "/mission " {
		t.Fatalf("CompleteText = %q, %v, want %q", text, ok, "/mission ")
	}

	p.Move(1)
	if e, _ := p.Selected(); e.Name != "model" {
		t.Fatalf("after Move(1) = %q", e.Name)
	}
	p.Move(5) // clamps at the end rather than wrapping
	if e, _ := p.Selected(); e.Name != "model" {
		t.Fatalf("after Move(5) = %q, want the last entry", e.Name)
	}
	p.Move(-9) // clamps at the top
	if e, _ := p.Selected(); e.Name != "mission" {
		t.Fatalf("after Move(-9) = %q, want the first entry", e.Name)
	}

	// A changed query returns the selection to the top: the row under it
	// means something else now.
	p.Move(1)
	p.SetQuery("")
	if e, _ := p.Selected(); e.Name != "compact" {
		t.Fatalf("after SetQuery the selection is %q, want the first entry", e.Name)
	}

	// Closed: no selection, no completion, no rows — Enter falls through to
	// an ordinary submission.
	p.Close()
	if _, ok := p.Selected(); ok {
		t.Fatal("closed palette still reports a selection")
	}
	if _, ok := p.CompleteText(); ok {
		t.Fatal("closed palette still completes")
	}
	if got := p.Render(80, 8, false); got != nil {
		t.Fatalf("closed palette rendered %d lines", len(got))
	}
	if p.IsOpen() {
		t.Fatal("IsOpen after Close")
	}
}

// TestUnit_ArgHint: the agent's own hint comes back verbatim once a name and
// a space are typed, and not before.
func TestUnit_ArgHint(t *testing.T) {
	p := appPalette(t)

	cases := []struct {
		buffer string
		want   string
		ok     bool
	}{
		{"", "", false},
		{"/", "", false},
		{"/mission", "", false}, // no space yet
		{"/mission ", "<agent> <objective>", true},
		{"/mission reviewer audit the ", "<agent> <objective>", true},
		{"  /policy strict", "<preset>", true},
		{"/help ", "", false},  // registered without a hint
		{"/model ", "", false}, // advertised without an Input
		{"/nosuchcommand args", "", false},
		{"plain text /mission ", "", false}, // not a command line at all
	}
	for _, c := range cases {
		got, ok := p.ArgHint(c.buffer)
		if got != c.want || ok != c.ok {
			t.Fatalf("ArgHint(%q) = %q, %v; want %q, %v", c.buffer, got, ok, c.want, c.ok)
		}
	}

	// The hint survives the palette being closed: the operator has moved on
	// to typing arguments.
	p.Close()
	if got, ok := p.ArgHint("/mission "); !ok || got != "<agent> <objective>" {
		t.Fatalf("ArgHint after Close = %q, %v", got, ok)
	}
}

// TestUnit_LookupIsTheDispatchSplit: locals resolve local, remotes resolve
// remote, unknown names resolve to nothing.
func TestUnit_LookupIsTheDispatchSplit(t *testing.T) {
	p := appPalette(t)

	if e, ok := p.Lookup("/quit"); !ok || !e.Local {
		t.Fatalf("/quit = %+v, %v; want a local", e, ok)
	}
	if e, ok := p.Lookup("mission"); !ok || e.Local {
		t.Fatalf("/mission = %+v, %v; want a remote", e, ok)
	}
	if e, ok := p.Lookup("/help"); !ok || !e.Local {
		t.Fatalf("/help = %+v, %v; want the shadowing local", e, ok)
	}
	if _, ok := p.Lookup("/definitely-not-a-command"); ok {
		t.Fatal("unknown name resolved; it must fall through as prompt text")
	}
	if _, ok := p.Lookup(""); ok {
		t.Fatal("empty name resolved")
	}
}

// TestUnit_RenderGoldens pins the overlay's rendering — full set, filtered, scrolled footer, and empty state — at every width and glyph variant.
func TestUnit_RenderGoldens(t *testing.T) {
	variants := []struct {
		name    string
		query   string
		move    int
		maxRows int
	}{
		{"full", "", 0, 8},
		{"filtered", "m", 1, 8},
		{"footer", "", 0, 4},
		{"footer_scrolled", "", 6, 4},
		{"empty", "zzz", 0, 8},
	}
	for _, v := range variants {
		for _, ascii := range []bool{false, true} {
			for _, w := range goldenWidths {
				label := "unicode"
				if ascii {
					label = "ascii"
				}
				name := fmt.Sprintf("palette_%s_%s_w%d", v.name, label, w)
				t.Run(name, func(t *testing.T) {
					p := appPalette(t)
					p.Open(v.query)
					p.Move(v.move)
					testkit.Golden(t, name, testkit.EncodeLines(p.Render(w, v.maxRows, ascii)))
				})
			}
		}
	}
}

// TestUnit_RenderNeverExceedsWidth: no rendered line exceeds width, even with unbounded agent-authored descriptions.
func TestUnit_RenderNeverExceedsWidth(t *testing.T) {
	long := New()
	long.SetRemote([]libacp.AvailableCommand{
		{Name: strings.Repeat("verylongname", 6), Description: strings.Repeat("and a description that keeps going ", 8)},
		{Name: "mission", Description: strings.Repeat("wide 東京 ", 20)},
		{Name: "a", Description: ""},
	})
	long.MustRegisterLocal("quit", strings.Repeat("leave ", 40), "")

	for _, ascii := range []bool{false, true} {
		for _, q := range []string{"", "m", "zzz"} {
			long.Open(q)
			for _, sel := range []int{0, 2} {
				long.SetQuery(q)
				long.Move(sel)
				for w := 4; w <= 140; w++ {
					for i, l := range long.Render(w, 3, ascii) {
						if got := textwidth.Width(l.Text()); got > w {
							t.Fatalf("ascii=%v query=%q width %d line %d: %d cells (%q)", ascii, q, w, i, got, l.Text())
						}
					}
				}
			}
		}
	}
}

// TestUnit_RenderFooterCountsHiddenRows: maxRows is the total budget including the footer, which counts hidden rows on both sides of the scroll window.
func TestUnit_RenderFooterCountsHiddenRows(t *testing.T) {
	p := appPalette(t) // 7 commands
	p.Open("")

	if lines := p.Render(80, 7, false); len(lines) != 7 {
		t.Fatalf("exactly-fitting set rendered %d lines, want 7 with no footer", len(lines))
	}
	lines := p.Render(80, 4, false)
	if len(lines) != 4 {
		t.Fatalf("rendered %d lines, want 4 — the footer lives inside the budget", len(lines))
	}
	// Three rows shown from the top, so four commands are below the window
	// and nothing at all is above it.
	if got := lines[3].Text(); got != "  +4 more" {
		t.Fatalf("footer = %q, want %q", got, "  +4 more")
	}

	// The selection always stays visible: the window scrolls to it. At the
	// end of the list the footer counts what is above instead, since that is
	// the only hidden half left.
	p.Move(6)
	lines = p.Render(80, 4, false)
	if len(lines) != 4 {
		t.Fatalf("scrolled render = %d lines, want 4", len(lines))
	}
	if !strings.Contains(lines[2].Text(), "/quit") {
		t.Fatalf("scrolled window ends with %q, want the selected /quit", lines[2].Text())
	}
	if got := lines[3].Text(); got != "  ↑4 above" {
		t.Fatalf("scrolled footer = %q, want %q", got, "  ↑4 above")
	}

	// Mid-list: both directions, in one line, in reading order.
	p.SetQuery("")
	p.Move(4)
	lines = p.Render(80, 4, false)
	if got := lines[3].Text(); got != "  ↑2 above  +2 more" {
		t.Fatalf("mid-list footer = %q, want %q", got, "  ↑2 above  +2 more")
	}

	// A one-line budget spends it on the selection rather than on a footer
	// with nothing above it.
	p.SetQuery("")
	one := p.Render(80, 1, false)
	if len(one) != 1 || strings.Contains(one[0].Text(), " more") {
		t.Fatalf("maxRows=1 rendered %q, want a single command row", texts(one))
	}
}

// TestUnit_FooterASCIIMarker: the "more above" marker degrades to ASCII "^" in a Mono terminal.
func TestUnit_FooterASCIIMarker(t *testing.T) {
	p := appPalette(t)
	p.Open("")
	p.Move(6)
	lines := p.Render(80, 4, true)
	got := lines[len(lines)-1].Text()
	if got != "  ^4 above" {
		t.Fatalf("ascii footer = %q, want %q", got, "  ^4 above")
	}
	for _, r := range got {
		if r > 127 {
			t.Fatalf("non-ASCII rune %q in a mono footer: %q", r, got)
		}
	}
}

// TestUnit_RenderNeverExceedsMaxRows: rendered line count never exceeds maxRows, for any selection or row allowance.
func TestUnit_RenderNeverExceedsMaxRows(t *testing.T) {
	p := appPalette(t)
	for _, q := range []string{"", "m", "zzz"} {
		for _, maxRows := range []int{1, 2, 3, 4, 7, 8, 20} {
			for sel := 0; sel < 9; sel++ {
				p.Open(q)
				p.Move(sel)
				for _, w := range []int{4, 20, 80} {
					if got := len(p.Render(w, maxRows, false)); got > maxRows {
						t.Fatalf("query=%q maxRows=%d sel=%d width=%d: %d lines",
							q, maxRows, sel, w, got)
					}
				}
			}
		}
	}
}

// TestUnit_RenderEmptyStateExplainsFallthrough: an unmatched filter renders one line saying Enter still sends as chat.
func TestUnit_RenderEmptyStateExplainsFallthrough(t *testing.T) {
	p := appPalette(t)
	p.Open("zzz")
	lines := p.Render(80, 8, false)
	if len(lines) != 1 {
		t.Fatalf("empty state rendered %d lines, want 1", len(lines))
	}
	if !strings.Contains(lines[0].Text(), "Enter sends as chat") {
		t.Fatalf("empty state = %q", lines[0].Text())
	}
	if got := p.Render(0, 8, false); got != nil {
		t.Fatal("zero width rendered rows")
	}
	if got := p.Render(80, 0, false); got != nil {
		t.Fatal("zero maxRows rendered rows")
	}
}

// modelDomain stands in for one session's advertised /model values: eight
// entries, enough to overflow a four-row budget and exercise both prefix and
// substring filtering.
func modelDomain() []string {
	return []string{
		"gpt-5-mini",
		"gpt-5",
		"claude-sonnet-4",
		"claude-opus-4",
		"gemini-3.1-pro",
		"llama-4-scout",
		"qwen3-coder",
		"devstral-small",
	}
}

// valuePalette is appPalette with /model and /policy value domains set.
func valuePalette(t *testing.T) *Palette {
	t.Helper()
	p := appPalette(t)
	p.SetValueDomains(map[string][]string{
		"model":  modelDomain(),
		"policy": {"hitl-policy-strict.json", "hitl-policy-dev.json"},
	})
	return p
}

// TestUnit_ValueModeNeedsBothASpaceAndADomain: value mode only activates once a command with a domain has its trailing space typed.
func TestUnit_ValueModeNeedsBothASpaceAndADomain(t *testing.T) {
	p := valuePalette(t)

	// A name alone is still the command menu, even for a command with a domain.
	p.Open("/model")
	if got := p.FilteredValues(); got != nil {
		t.Fatalf("value mode without a space: %v", got)
	}
	if names(p.Filtered()); len(p.Filtered()) != 1 {
		t.Fatalf("command filter = %v, want just /model", names(p.Filtered()))
	}

	// The space arms it.
	p.SetQuery("/model ")
	if got := p.FilteredValues(); !equal(got, modelDomain()) {
		t.Fatalf("values = %v, want the whole domain", got)
	}

	// A command without a domain is untouched by all of this: its arguments
	// are free text, and the overlay keeps listing commands.
	p.SetQuery("/compact 12")
	if got := p.FilteredValues(); got != nil {
		t.Fatalf("/compact has no domain but offered %v", got)
	}
	if _, ok := p.CompleteText(); !ok {
		t.Fatal("/compact stopped completing once value mode existed")
	}

	// Neither is an unknown command: nothing to complete, nothing blocked.
	p.SetQuery("/definitely-not-a-command x")
	if got := p.FilteredValues(); got != nil {
		t.Fatalf("unknown command offered values: %v", got)
	}

	// Past the first argument the domain is done talking: an agent-name
	// domain must not keep filtering once the objective is being typed.
	p.SetQuery("/model gpt-5 and then some")
	if got := p.FilteredValues(); got != nil {
		t.Fatalf("second argument still in value mode: %v", got)
	}

	// Closing leaves value mode with everything else.
	p.SetQuery("/model gpt")
	p.Close()
	if got := p.FilteredValues(); got != nil {
		t.Fatalf("closed palette offered values: %v", got)
	}
	if _, ok := p.SelectedValue(); ok {
		t.Fatal("closed palette reports a selected value")
	}
}

// TestUnit_ValueFilterIsPrefixThenSubstring: value matches rank prefix matches before substring matches, case-insensitively.
func TestUnit_ValueFilterIsPrefixThenSubstring(t *testing.T) {
	p := valuePalette(t)
	p.Open("/model c")

	want := []string{
		// prefix matches, in the server's order …
		"claude-sonnet-4", "claude-opus-4",
		// … then substring matches, in the server's order.
		"llama-4-scout", "qwen3-coder",
	}
	if got := p.FilteredValues(); !equal(got, want) {
		t.Fatalf("values = %v, want %v", got, want)
	}

	// Case-insensitive, like the command filter.
	p.SetQuery("/model CLAUDE-OPUS")
	if got := p.FilteredValues(); !equal(got, []string{"claude-opus-4"}) {
		t.Fatalf("case-insensitive filter = %v", got)
	}

	// Nothing matched is not an error: the value is simply not one the server
	// advertised, and the line still sends.
	p.SetQuery("/model zzz")
	if got := p.FilteredValues(); len(got) != 0 {
		t.Fatalf("over-filtered values = %v", got)
	}
}

// TestUnit_ValueCompletionWritesTheValue: Tab in value mode writes the command plus the selected value, and leaves an unmatched partial untouched.
func TestUnit_ValueCompletionWritesTheValue(t *testing.T) {
	p := valuePalette(t)
	p.Open("/model cla")

	value, ok := p.SelectedValue()
	if !ok || value != "claude-sonnet-4" {
		t.Fatalf("SelectedValue = %q, %v", value, ok)
	}
	text, ok := p.CompleteValueText()
	if !ok || text != "/model claude-sonnet-4" {
		t.Fatalf("CompleteValueText = %q, %v", text, ok)
	}
	// The same key does both halves, so the app needs no new binding.
	if got, _ := p.CompleteText(); got != text {
		t.Fatalf("CompleteText = %q, want the value completion %q", got, text)
	}

	// Re-completing is idempotent: the buffer is still "name value", so a second
	// Tab re-selects the same value instead of wiping the argument.
	p.SetQuery(text)
	if got, ok := p.CompleteText(); !ok || got != text {
		t.Fatalf("second Tab = %q, %v; want %q unchanged", got, ok, text)
	}

	// Down walks the values, not the commands.
	p.SetQuery("/model ")
	p.Move(2)
	if got, _ := p.SelectedValue(); got != "claude-sonnet-4" {
		t.Fatalf("after Move(2) the value is %q", got)
	}
	p.Move(99) // clamps at the last value rather than wrapping
	if got, _ := p.SelectedValue(); got != "devstral-small" {
		t.Fatalf("after Move(99) the value is %q", got)
	}

	// A partial with no match completes nothing: Tab may not delete what the
	// operator typed just because the runtime has not heard of it.
	p.SetQuery("/model zzz")
	if got, ok := p.CompleteText(); ok {
		t.Fatalf("unmatched partial completed to %q", got)
	}
}

// TestUnit_SetValueDomainsReplacesAndFollowsTheSelection: SetValueDomains replaces rather than merges, and the selection follows its value by name.
func TestUnit_SetValueDomainsReplacesAndFollowsTheSelection(t *testing.T) {
	p := valuePalette(t)
	p.Open("/model ")
	p.Move(3) // claude-opus-4

	// A new set that still contains the selected model keeps it selected, even
	// though it moved two rows up.
	p.SetValueDomains(map[string][]string{"model": {"gpt-5", "claude-opus-4", "gpt-5-mini"}})
	if got, _ := p.SelectedValue(); got != "claude-opus-4" {
		t.Fatalf("selection after replacement = %q, want it to follow the value", got)
	}

	// Replaces, not merges: a model the session no longer offers is gone.
	if got := p.FilteredValues(); !equal(got, []string{"gpt-5", "claude-opus-4", "gpt-5-mini"}) {
		t.Fatalf("values = %v, want only the new set", got)
	}

	// An empty domain — or none at all — turns value mode off rather than
	// showing an empty list.
	p.SetValueDomains(map[string][]string{"model": {}, "": {"x"}})
	if got := p.FilteredValues(); got != nil {
		t.Fatalf("empty domain still in value mode: %v", got)
	}
	p.SetValueDomains(nil)
	if got := p.FilteredValues(); got != nil {
		t.Fatalf("nil domains still in value mode: %v", got)
	}
}

// TestUnit_ValueDomainsAreSanitized: value-domain strings are sanitized, deduplicated and trimmed before they reach the overlay.
func TestUnit_ValueDomainsAreSanitized(t *testing.T) {
	p := appPalette(t)
	p.SetValueDomains(map[string][]string{
		"model": {"gpt\x1b[31m-5", "  spaced  ", "", "dup", "dup"},
	})
	p.Open("/model ")
	got := p.FilteredValues()
	want := []string{"gpt-5", "spaced", "dup"}
	if !equal(got, want) {
		t.Fatalf("sanitized values = %q, want %q", got, want)
	}
	for _, v := range got {
		if strings.ContainsRune(v, 0x1b) {
			t.Fatalf("escape survived in %q", v)
		}
	}
}

// TestUnit_ArgHintCarriesTheDomainSize: ArgHint appends the domain's true size to the agent's hint, or stands alone without one.
func TestUnit_ArgHintCarriesTheDomainSize(t *testing.T) {
	p := valuePalette(t)

	cases := []struct {
		buffer string
		want   string
		ok     bool
	}{
		// /model is advertised without an Input hint, so the count is the whole
		// line — it says something where there was nothing before.
		{"/model ", "(8 values)", true},
		{"/model gpt-5", "(8 values)", true},
		// /policy has both: the agent's own hint, verbatim, then the size.
		{"/policy ", "<preset> (2 values)", true},
		// A command with a hint and no domain is byte-identical to before.
		{"/mission ", "<agent> <objective>", true},
		// Neither hint nor domain, and non-commands, stay silent.
		{"/help ", "", false},
		{"/nosuchcommand args", "", false},
		{"/model", "", false},
	}
	for _, c := range cases {
		got, ok := p.ArgHint(c.buffer)
		if got != c.want || ok != c.ok {
			t.Fatalf("ArgHint(%q) = %q, %v; want %q, %v", c.buffer, got, ok, c.want, c.ok)
		}
	}
}

// TestUnit_RenderValueGoldens pins the value overlay's rendering — whole domain, scrolled footer, filtered, unmatched, and no-domain fallback.
func TestUnit_RenderValueGoldens(t *testing.T) {
	variants := []struct {
		name    string
		query   string
		move    int
		maxRows int
	}{
		{"value_model", "/model ", 0, 8},
		{"value_model_footer", "/model ", 0, 5},
		{"value_filtered", "/model c", 1, 8},
		{"value_unmatched", "/model zzz", 0, 8},
		{"value_no_domain", "/compact 12", 0, 8},
	}
	for _, v := range variants {
		for _, ascii := range []bool{false, true} {
			for _, w := range goldenWidths {
				label := "unicode"
				if ascii {
					label = "ascii"
				}
				name := fmt.Sprintf("palette_%s_%s_w%d", v.name, label, w)
				t.Run(name, func(t *testing.T) {
					p := valuePalette(t)
					p.Open(v.query)
					p.Move(v.move)
					testkit.Golden(t, name, testkit.EncodeLines(p.Render(w, v.maxRows, ascii)))
				})
			}
		}
	}
}

// TestUnit_ValueRenderRespectsTheBudget: value-mode rendering also never exceeds maxRows or width.
func TestUnit_ValueRenderRespectsTheBudget(t *testing.T) {
	p := valuePalette(t)
	for _, q := range []string{"/model ", "/model c", "/model zzz"} {
		for _, maxRows := range []int{1, 2, 3, 5, 8, 20} {
			for sel := 0; sel < 9; sel++ {
				p.Open(q)
				p.Move(sel)
				for _, w := range []int{4, 20, 80} {
					lines := p.Render(w, maxRows, false)
					if len(lines) > maxRows {
						t.Fatalf("query=%q maxRows=%d sel=%d width=%d: %d lines", q, maxRows, sel, w, len(lines))
					}
					for i, l := range lines {
						if got := textwidth.Width(l.Text()); got > w {
							t.Fatalf("query=%q width %d line %d: %d cells (%q)", q, w, i, got, l.Text())
						}
					}
				}
			}
		}
	}
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

func texts(lines []frame.Line) []string {
	out := make([]string, 0, len(lines))
	for _, l := range lines {
		out = append(out, l.Text())
	}
	return out
}

package approval

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/contenox/contenox/internal/services/approvalflow"
	"github.com/contenox/contenox/internal/surfaces/beamtui/enginebridge"
	"github.com/contenox/contenox/internal/surfaces/beamtui/frame"
	"github.com/contenox/contenox/internal/surfaces/beamtui/testkit"
	"github.com/contenox/contenox/internal/surfaces/beamtui/textwidth"
	libacp "github.com/contenox/libacp"
)

// goldenWidths is the resize matrix: narrow, default terminal, wide.
var goldenWidths = []int{60, 80, 120}

const sampleDiff = `--- a/internal/app.go
+++ b/internal/app.go
@@ -12,3 +12,3 @@
-    return errors.New("todo")
+    return run(ctx, cfg)`

// sampleEvent is a realistic gated write: three arguments (one past the
// display cap), a named policy rule, and a short diff.
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
			ToolsName:   "local_fs",
			ToolName:    "write_file",
			PolicyName:  "guarded",
			PolicyPath:  "rules[3].local_fs.write_file",
			MatchedRule: intp(4),
			Diff:        sampleDiff,
		},
		RawInput: raw,
		Options: []libacp.PermissionOption{
			{OptionID: approvalflow.OptionAllow, Name: "Allow", Kind: libacp.PermissionAllowOnce},
			{OptionID: approvalflow.OptionDeny, Name: "Deny", Kind: libacp.PermissionRejectOnce},
		},
		Resolve: resolve,
	}
}

// sampleScriptCode is a multi-line argument: real source with a tab indent
// and a literal "\n" inside a string, so a golden that re-escapes the value
// gives itself away.
const sampleScriptCode = `const notes = host.tool("local_fs.read_file", { path: "CHANGELOG.md" });
if (!notes) {
	throw new Error("CHANGELOG.md is empty");
}
return { lines: notes.split("\n").length };`

// scriptEvent is a goja call whose decisive argument is code, with no diff to
// fall back on and a declared reach as the only signal of what it will
// touch. mayCall/declared are parameters so one fixture drives all three
// states of the declaration.
func scriptEvent(mayCall []string, declared *bool) enginebridge.PermissionRequested {
	args := map[string]any{
		"code":       sampleScriptCode,
		"timeout_ms": float64(2000),
	}
	raw, err := json.Marshal(args)
	if err != nil {
		panic(err)
	}
	return enginebridge.PermissionRequested{
		SessionID:  libacp.SessionID("sess-7f3a"),
		ToolCallID: "goja.goja_eval",
		Title:      "goja.goja_eval",
		Kind:       libacp.ToolKindOther,
		Status:     libacp.ToolCallStatusPending,
		Meta: approvalflow.Meta{
			ToolsName:       "goja",
			ToolName:        "goja_eval",
			PolicyName:      "guarded",
			PolicyPath:      "rules[2].goja",
			MayCall:         mayCall,
			MayCallDeclared: declared,
		},
		RawInput: raw,
		Options: []libacp.PermissionOption{
			{OptionID: approvalflow.OptionAllow, Name: "Allow", Kind: libacp.PermissionAllowOnce},
			{OptionID: approvalflow.OptionDeny, Name: "Deny", Kind: libacp.PermissionRejectOnce},
		},
		Resolve: func(bool) {},
	}
}

func boolp(v bool) *bool { return &v }
func intp(v int) *int    { return &v }

func texts(lines []frame.Line) []string {
	out := make([]string, 0, len(lines))
	for _, l := range lines {
		out = append(out, l.Text())
	}
	return out
}

// TestUnit_CardGoldens pins the whole card's rendering, in every state and
// glyph variant, in its fixed order: identity, sorted args, policy, diff, decision.
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

// TestUnit_ArgsAreSortedAndSummarised: arguments render in stable sorted order, and a long value elides to one line with its true size visible.
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
	// The body is elided with its size shown, not silently.
	if !strings.Contains(args[0], "bytes") || !strings.Contains(args[0], "lines") {
		t.Fatalf("long arg lacks a visible elision marker: %q", args[0])
	}
	if strings.Contains(args[0], "\n") {
		t.Fatalf("long arg leaked a newline into a span: %q", args[0])
	}

	if want := "policy guarded · path rules[3].local_fs.write_file · rule 5"; lines[5] != want {
		t.Fatalf("policy line = %q, want %q", lines[5], want)
	}
}

// TestUnit_PolicyLineShowsMatchedRuleOrDefault: MatchedRule renders 1-based,
// so "rule 1" reads as an ordinal no one can mistake for the wire's 0-based
// index; nil beside a known policy name reads as "no rule matched", never a
// bare rule number, since no rule matching is exactly what nil means. A
// MatchedRule with no policy name withholds the rule claim entirely — it
// would be an index into a document the card never named.
func TestUnit_PolicyLineShowsMatchedRuleOrDefault(t *testing.T) {
	policyLine := func(ev enginebridge.PermissionRequested) string {
		for _, l := range texts(New(ev).Render(200, false, "")) {
			if strings.HasPrefix(l, "policy ") || strings.HasPrefix(l, "path ") {
				return l
			}
		}
		return ""
	}

	t.Run("matched rule present, shown 1-based", func(t *testing.T) {
		ev := sampleEvent(nil)
		ev.Meta.MatchedRule = intp(0)
		if got, want := policyLine(ev), "policy guarded · path rules[3].local_fs.write_file · rule 1"; got != want {
			t.Fatalf("policy line = %q, want %q", got, want)
		}
	})

	t.Run("nil matched rule reads as no rule matched, never rule 0", func(t *testing.T) {
		ev := sampleEvent(nil)
		ev.Meta.MatchedRule = nil
		got := policyLine(ev)
		if want := "policy guarded · path rules[3].local_fs.write_file · no rule matched"; got != want {
			t.Fatalf("policy line = %q, want %q", got, want)
		}
		if strings.Contains(got, "rule 0") {
			t.Fatalf("policy line = %q, nil must never render as rule 0", got)
		}
	})

	t.Run("no policy name withholds the rule claim even if MatchedRule is set", func(t *testing.T) {
		ev := sampleEvent(nil)
		ev.Meta.PolicyName = ""
		ev.Meta.MatchedRule = intp(0)
		if got, want := policyLine(ev), "path rules[3].local_fs.write_file"; got != want {
			t.Fatalf("policy line = %q, want %q (no rule/default claim without a named policy)", got, want)
		}
	})

	t.Run("detail present displaces the rule index", func(t *testing.T) {
		ev := sampleEvent(nil)
		ev.Meta.Detail = `shell command "rm" matched command_ask_always`
		want := `policy guarded · path rules[3].local_fs.write_file · shell command "rm" matched command_ask_always`
		if got := policyLine(ev); got != want {
			t.Fatalf("policy line = %q, want %q", got, want)
		}
	})

	t.Run("detail absent falls back to the rule index", func(t *testing.T) {
		ev := sampleEvent(nil)
		ev.Meta.Detail = ""
		if got, want := policyLine(ev), "policy guarded · path rules[3].local_fs.write_file · rule 5"; got != want {
			t.Fatalf("policy line = %q, want %q", got, want)
		}
	})

	t.Run("detail present with nil MatchedRule still shows the detail, not no rule matched", func(t *testing.T) {
		ev := sampleEvent(nil)
		ev.Meta.MatchedRule = nil
		ev.Meta.Detail = `shell command "rm" matched command_ask_always`
		want := `policy guarded · path rules[3].local_fs.write_file · shell command "rm" matched command_ask_always`
		if got := policyLine(ev); got != want {
			t.Fatalf("policy line = %q, want %q", got, want)
		}
	})
}

// TestUnit_DiffIsLast: the diff sits immediately above the decision line, and its lines are styled by their own first character.
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

// TestUnit_DiffTruncationWarnsWithExactCounts: a diff past maxDiffLines is cut with an exact-count warning; one exactly at the cap warns of nothing.
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
	// The cut is the first 120 lines: body, then warning, then decision.
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

// TestUnit_TruncationWarningSurvivesNarrowWidths: the warning wraps rather than eliding, reassembling to the same sentence at every width.
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

// TestUnit_DiffLinesAreUnwrapped: a diff line is emitted verbatim as one span even when wider than the terminal, never wrapped or elided.
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

// TestUnit_DiffLinesAreSanitized: a diff line's exemption from wrapping and elision does not extend to control or bidi characters — those are stripped.
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

// TestUnit_TabIndentSurvivesAtEveryWidth: a diff line's tab-expanded indentation is the same string, as one unwrapped span, at every width.
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

// TestUnit_CardChromeIsSanitized: every peer-supplied string on the card, not just the diff, is stripped of control and bidi characters.
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

// TestUnit_ResolveCallsTheBridgeExactlyOnce: a doubled keystroke, or a resolve after a cancel, never answers the bridge twice.
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

// TestUnit_CancelledCardStopsWaiting: a cancelled turn flips its pending card rather than leaving it spinning, without answering the bridge.
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

// TestUnit_PendingFooterCarriesTheSpinner: the pending footer shows the spinner when given one, and degrades to keys alone without it.
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

// TestUnit_NoDiffFallsBackToANewContentSummary: no diff and new content summarizes as a line count, never a blank section or a content dump.
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

// TestUnit_RawInputShapes: a non-object payload is shown rather than dropped, and an absent one produces no argument rows at all.
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

// TestUnit_ToolIdentityFallsBackToTitle: a peer that sent no _meta still gets a card naming what it is asking about, via Title then "unknown tool".
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

// TestUnit_RenderNeverExceedsWidth: no rendered line exceeds width, except diff bodies, whose exemption is tested separately.
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

// TestUnit_ArgBlockGoldens pins the multi-line argument block's rendering at the resize matrix, in both glyph sets.
func TestUnit_ArgBlockGoldens(t *testing.T) {
	for _, ascii := range []bool{false, true} {
		for _, w := range goldenWidths {
			label := "unicode"
			if ascii {
				label = "ascii"
			}
			name := fmt.Sprintf("argblock_%s_w%d", label, w)
			t.Run(name, func(t *testing.T) {
				c := New(scriptEvent([]string{"local_fs.read_file", "local_shell.exec"}, boolp(true)))
				testkit.Golden(t, name, testkit.EncodeLines(c.Render(w, ascii, "⠋")))
			})
		}
	}
}

// TestUnit_MultiLineArgIsABlockNotAnEscapedOneLiner: every source line gets its own frame line at the block indent, with no "\n" and nothing cut mid-content.
func TestUnit_MultiLineArgIsABlockNotAnEscapedOneLiner(t *testing.T) {
	lines := texts(New(scriptEvent(nil, nil)).Render(120, false, ""))
	joined := strings.Join(lines, "\n")

	// The scalar shape's own marker is what the escaped one-liner leaves behind.
	if strings.Contains(joined, "bytes,") {
		t.Fatalf("the code argument is still summarised onto one line:\n%s", joined)
	}
	// There is no diff on this card, so nothing on it may point at one.
	if strings.Contains(joined, "see diff") {
		t.Fatalf("the code argument points at a diff that does not exist:\n%s", joined)
	}

	// The key line opens the block; the body follows it in source order.
	start := -1
	for i, l := range lines {
		if l == "  code =" {
			start = i
			break
		}
	}
	if start < 0 {
		t.Fatalf("no block header for the code argument: %q", lines)
	}
	want := strings.Split(sampleScriptCode, "\n")
	for i, src := range want {
		// The tab line is the one that must arrive expanded, not folded.
		expect := "    " + strings.ReplaceAll(src, "\t", "        ")
		if got := lines[start+1+i]; got != expect {
			t.Fatalf("block line %d = %q, want %q", i, got, expect)
		}
	}
	// Sorted-key order still holds around the block.
	if got := lines[start+1+len(want)]; got != "  timeout_ms = 2000" {
		t.Fatalf("line after the block = %q, want the next argument", got)
	}
	// Copy fidelity in the other direction: a backslash-n that is part of the
	// source stays a backslash-n. The block neither escapes newlines nor
	// un-escapes what the author wrote.
	if got := lines[start+len(want)]; got != `    return { lines: notes.split("\n").length };` {
		t.Fatalf("the source's own escape was rewritten: %q", got)
	}

	// And the body is styled as code, one span per line, so it copies clean.
	rendered := New(scriptEvent(nil, nil)).Render(120, false, "")
	for i := range want {
		l := rendered[start+1+i]
		if len(l) != 1 || l[0].Style != frame.StyleCode {
			t.Fatalf("block line %d has %d spans styled %q, want one code span", i, len(l), l[0].Style)
		}
	}
}

// TestUnit_ArgBlockLinesAreUnwrapped: a block's code line is emitted whole at any width, same as a diff body line.
func TestUnit_ArgBlockLinesAreUnwrapped(t *testing.T) {
	long := "const payload = \"" + strings.Repeat("x", 300) + "\";"
	ev := scriptEvent(nil, nil)
	ev.RawInput = mustArgs(t, map[string]any{"code": "// head\n" + long})

	for _, w := range []int{20, 40, 80} {
		found := false
		for _, l := range New(ev).Render(w, false, "") {
			if l.Text() == "    "+long {
				found = true
				if len(l) != 1 {
					t.Fatalf("width %d: block line split into %d spans", w, len(l))
				}
			}
		}
		if !found {
			t.Fatalf("width %d: the 300-cell block line was not emitted verbatim", w)
		}
	}
}

// TestUnit_ArgBlockIsSanitized: a block body's exemption from wrapping does not extend to control or bidi characters, which are stripped.
func TestUnit_ArgBlockIsSanitized(t *testing.T) {
	ev := scriptEvent(nil, nil)
	ev.RawInput = mustArgs(t, map[string]any{
		"code": "run()\x1b[2Jcleared\n‮drawkcab\n\x1b]0;pwned\x07gone\n\tindented",
	})

	var body []string
	for _, l := range New(ev).Render(120, false, "") {
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
		if strings.HasPrefix(l.Text(), "    ") {
			body = append(body, l.Text())
		}
	}
	want := []string{"    run()cleared", "    drawkcab", "    gone", "            indented"}
	if !equal(body, want) {
		t.Fatalf("block body = %q, want %q", body, want)
	}
}

// TestUnit_ArgBlockCapAnnouncesWhatIsHidden: a block past maxArgBlockLines is cut with a wrapped, exact-count cap notice; one at the cap announces nothing.
func TestUnit_ArgBlockCapAnnouncesWhatIsHidden(t *testing.T) {
	body := make([]string, 140)
	for i := range body {
		body[i] = fmt.Sprintf("line %d", i)
	}
	ev := scriptEvent(nil, nil)
	ev.RawInput = mustArgs(t, map[string]any{"code": strings.Join(body, "\n")})

	lines := texts(New(ev).Render(120, false, ""))
	shown := 0
	for _, l := range lines {
		if strings.HasPrefix(l, "    line ") {
			shown++
		}
	}
	if shown != maxArgBlockLines {
		t.Fatalf("rendered %d block lines, want %d", shown, maxArgBlockLines)
	}
	if got := lines[maxArgBlockLines+2]; got != "    line 39" {
		t.Fatalf("last shown block line = %q, want the 40th", got)
	}
	warning := lines[maxArgBlockLines+3]
	for _, want := range []string{"⚠", "+100 more lines", "approving accepts content you have not seen"} {
		if !strings.Contains(warning, want) {
			t.Fatalf("cap notice = %q, missing %q", warning, want)
		}
	}

	for _, ascii := range []bool{false, true} {
		for _, w := range []int{40, 60, 80, 120} {
			var warn strings.Builder
			for _, l := range New(ev).Render(w, ascii, "") {
				if l[0].Style == frame.StyleWarn {
					warn.WriteString(l.Text())
				}
			}
			if got := warn.String(); got != blockTruncationWarning(100, ascii) {
				t.Fatalf("ascii=%v width %d: notice reassembles to %q, want %q",
					ascii, w, got, blockTruncationWarning(100, ascii))
			}
		}
	}

	// A body exactly at the cap does not announce a cap.
	ev.RawInput = mustArgs(t, map[string]any{"code": strings.Join(body[:maxArgBlockLines], "\n")})
	for _, l := range texts(New(ev).Render(120, false, "")) {
		if strings.Contains(l, "more lines") {
			t.Fatalf("a body exactly at the cap announced one: %q", l)
		}
	}
}

// TestUnit_ArgBlockYieldsToTheDiff: when a diff is rendered, the argument stays a scalar "see diff" summary instead of duplicating the body as a block.
func TestUnit_ArgBlockYieldsToTheDiff(t *testing.T) {
	lines := texts(New(sampleEvent(nil)).Render(120, false, ""))
	if !strings.HasPrefix(lines[2], "  content = ") || !strings.Contains(lines[2], "see diff") {
		t.Fatalf("a write_file card lost its scalar summary: %q", lines[2])
	}
	if lines[2] == "  content =" {
		t.Fatal("the write body was blocked out above its own diff")
	}
}

// TestUnit_MayCallLine: the three states of Meta.MayCallDeclared each render a different reach line, and the list is capped, sanitized and deduplicated.
func TestUnit_MayCallLine(t *testing.T) {
	line := func(ev enginebridge.PermissionRequested, w int, ascii bool) string {
		for _, l := range texts(New(ev).Render(w, ascii, "")) {
			if strings.HasPrefix(l, "may call") {
				return l
			}
		}
		return ""
	}

	t.Run("present", func(t *testing.T) {
		ev := scriptEvent([]string{"local_fs.read_file", "local_shell.exec"}, boolp(true))
		if got, want := line(ev, 120, false), "may call  local_fs.read_file · local_shell.exec"; got != want {
			t.Fatalf("reach line = %q, want %q", got, want)
		}
		if got, want := line(ev, 120, true), "may call  local_fs.read_file - local_shell.exec"; got != want {
			t.Fatalf("ascii reach line = %q, want %q", got, want)
		}
		// It sits with the identity it qualifies, directly under the tool line.
		lines := texts(New(ev).Render(120, false, ""))
		if !strings.HasPrefix(lines[1], "tool  ") || !strings.HasPrefix(lines[2], "may call") {
			t.Fatalf("reach line is not under the tool line: %q", lines[:4])
		}
	})

	t.Run("absent", func(t *testing.T) {
		// An ordinary tool card carried no declaration and must claim nothing.
		if got := line(sampleEvent(nil), 120, false); got != "" {
			t.Fatalf("a card with no declaration printed %q", got)
		}
		if got := line(scriptEvent(nil, nil), 120, false); got != "" {
			t.Fatalf("an undeclared-and-unknown reach printed %q", got)
		}
	})

	t.Run("declared empty is not the same as undeclared", func(t *testing.T) {
		if got, want := line(scriptEvent(nil, boolp(true)), 120, false), "may call  nothing"; got != want {
			t.Fatalf("declared-empty reach = %q, want %q", got, want)
		}
		if got, want := line(scriptEvent(nil, boolp(false)), 120, false),
			"may call  any tool the policy allows · nothing declared"; got != want {
			t.Fatalf("undeclared reach = %q, want %q", got, want)
		}
	})

	t.Run("capped", func(t *testing.T) {
		names := make([]string, 0, 20)
		for i := range 20 {
			names = append(names, fmt.Sprintf("tools_%02d.call", i))
		}
		got := line(scriptEvent(names, boolp(true)), 400, false)
		if strings.Count(got, "tools_") != maxMayCallNames {
			t.Fatalf("reach line listed %d names, want %d: %q", strings.Count(got, "tools_"), maxMayCallNames, got)
		}
		if !strings.HasSuffix(got, "+12 more") {
			t.Fatalf("capped reach line = %q, want it to end in the count it hid", got)
		}
	})

	t.Run("blanks and repeats do not pad the list", func(t *testing.T) {
		ev := scriptEvent([]string{"local_fs.read_file", "  ", "local_fs.read_file", ""}, boolp(true))
		if got, want := line(ev, 120, false), "may call  local_fs.read_file"; got != want {
			t.Fatalf("reach line = %q, want %q", got, want)
		}
	})

	t.Run("sanitized and clamped", func(t *testing.T) {
		ev := scriptEvent([]string{"a\x1b[2Jb\x1b]0;t\x07c\td\x7f‮e"}, boolp(true))
		for _, l := range New(ev).Render(60, false, "") {
			for _, sp := range l {
				for _, r := range sp.Text {
					if r < 0x20 || r == 0x7f {
						t.Fatalf("reach span %q carries %U", sp.Text, r)
					}
					if (r >= 0x202a && r <= 0x202e) || (r >= 0x2066 && r <= 0x2069) {
						t.Fatalf("reach span %q carries bidi control %U", sp.Text, r)
					}
				}
			}
			if got := textwidth.Width(l.Text()); strings.HasPrefix(l.Text(), "may call") && got > 60 {
				t.Fatalf("reach line is %d cells at width 60: %q", got, l.Text())
			}
		}
	})
}

// TestUnit_ArgBlockChromeNeverExceedsWidth: the block's unwrapped exemption covers its body only; the key line and cap notice still fit width.
func TestUnit_ArgBlockChromeNeverExceedsWidth(t *testing.T) {
	body := make([]string, 60)
	for i := range body {
		body[i] = strings.Repeat("東京 wide ", 8)
	}
	ev := scriptEvent([]string{"local_fs.read_file", "local_shell.exec"}, boolp(true))
	ev.RawInput = mustArgs(t, map[string]any{
		strings.Repeat("long_key_", 6): strings.Join(body, "\n"),
	})

	for _, ascii := range []bool{false, true} {
		for w := 4; w <= 140; w++ {
			for i, l := range New(ev).Render(w, ascii, "⠋") {
				if strings.HasPrefix(l.Text(), "    ") {
					continue // block body: unwrapped by design
				}
				if got := textwidth.Width(l.Text()); got > w {
					t.Fatalf("ascii=%v width %d line %d: %d cells (%q)", ascii, w, i, got, l.Text())
				}
			}
		}
	}
}

func mustArgs(t *testing.T, args map[string]any) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	return raw
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

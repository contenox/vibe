package transcript

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/contenox/contenox/internal/surfaces/beamtui/enginebridge"
	"github.com/contenox/contenox/internal/surfaces/beamtui/frame"
	"github.com/contenox/contenox/internal/surfaces/beamtui/testkit"
	"github.com/contenox/contenox/internal/surfaces/beamtui/textwidth"
	libacp "github.com/contenox/contenox/libacp"
)

// goldenWidths is the resize matrix: narrow, default, wide.
var goldenWidths = []int{60, 80, 120}

const (
	sess  = libacp.SessionID("sess-1")
	other = libacp.SessionID("sess-2")
)

// frameSpinner is a fixed spinner frame: animation is a pure function of the
// tick elsewhere, so a golden pins one frame and nothing wobbles.
func frameSpinner(ascii bool) string {
	if ascii {
		return "-"
	}
	return "⠙"
}

func text(id, s string) enginebridge.Event {
	return enginebridge.TextDelta{SessionID: sess, MessageID: id, Text: s}
}

func thought(id, s string) enginebridge.Event {
	return enginebridge.ThoughtDelta{SessionID: sess, MessageID: id, Text: s}
}

func user(s string) enginebridge.Event {
	return enginebridge.UserEcho{SessionID: sess, MessageID: "u1", Text: s}
}

func toolOpen(id, title string, kind libacp.ToolKind, st libacp.ToolCallStatus) enginebridge.Event {
	return enginebridge.ToolCallOpened{SessionID: sess, ToolCallID: id, Title: title, Kind: kind, Status: st}
}

func toolUpdate(id string, st libacp.ToolCallStatus) enginebridge.Event {
	return enginebridge.ToolCallUpdated{SessionID: sess, ToolCallID: id, Status: st}
}

func report(agent, kind, body string) enginebridge.Event {
	return enginebridge.MissionReport{
		SessionID: sess, MissionID: "mis-1", ReportID: "rep-1",
		Kind: kind, AgentName: agent, MessageID: "mission-report-rep-1", Text: body,
	}
}

func ask(agent, body string) enginebridge.Event {
	return enginebridge.MissionAsk{
		SessionID: sess, MissionID: "mis-1", AskID: "ask-1",
		AgentName: agent, MessageID: "mission-ask-ask-1", Summary: body, Text: body,
	}
}

func missionStatus(agent, from, to, reason string) enginebridge.Event {
	return enginebridge.MissionStatusChanged{
		SessionID: sess, MissionID: "mis-1", AgentName: agent,
		Old: from, New: to, Reason: reason,
		MessageID: "mission-status-mis-1-" + from + "-" + to,
	}
}

func missionPlan(agent string, rev int, explanation string, done, running, pending int) enginebridge.Event {
	return enginebridge.MissionPlanRevised{
		SessionID: sess, MissionID: "mis-1", AgentName: agent,
		Revision: rev, Explanation: explanation,
		EntryCount: done + running + pending,
		Pending:    pending, InProgress: running, Completed: done,
		MessageID: fmt.Sprintf("mission-plan-mis-1-%d", rev),
	}
}

func shellRun(cmd string) enginebridge.Event {
	return enginebridge.ShellRunStarted{SessionID: sess, Command: cmd}
}

func terminal(chunk string) enginebridge.Event {
	return enginebridge.TerminalChunk{SessionID: sess, Chunk: chunk}
}

func terminalReset(chunk string) enginebridge.Event {
	return enginebridge.TerminalChunk{SessionID: sess, Chunk: chunk, Reset: true}
}

func ended(r libacp.StopReason) enginebridge.Event {
	return enginebridge.TurnEnded{SessionID: sess, StopReason: r}
}

// apply plays a script into a fresh transcript.
func apply(evs ...enginebridge.Event) *Transcript {
	tr := New()
	for _, e := range evs {
		tr.Apply(e)
	}
	return tr
}

// commit is what the app hands the engine after a script: everything settled
// so far as scrollback, the in-progress tail as the live region.
func commit(tr *Transcript, width int, ascii bool) frame.Frame {
	return frame.Frame{
		Scrollback: tr.TakeAppends(width, ascii),
		Live:       tr.Live(width, ascii, frameSpinner(ascii)),
	}
}

// texts projects lines down to their copyable text.
func texts(lines []frame.Line) []string {
	out := make([]string, 0, len(lines))
	for _, l := range lines {
		out = append(out, l.Text())
	}
	return out
}

// Scenarios: hand-built event scripts rendered at every width in both glyph
// variants by the goldens below.

func streamScript() []enginebridge.Event {
	return []enginebridge.Event{
		user("wire up the transcript component"),
		thought("m0", "The flush policy is the part that matters.\n"),
		// Chunk boundaries fall mid-word and mid-sentence.
		text("m1", "I'll start with the flush poli"),
		text("m1", "cy, because it is what decides when a line can never change again.\n\n"),
		text("m1", "## Plan\n"),
		text("m1", "- settle a source line when its newline arrives\n"),
		text("m1", "- keep the unterminated tail in the live region\n\n"),
		toolOpen("t1", "Read internal/surfaces/beamtui/frame/frame.go", libacp.ToolKindRead, libacp.ToolCallStatusPending),
		toolUpdate("t1", libacp.ToolCallStatusInProgress),
		text("m1", "The tail below is still strea"),
	}
}

func markdownScript() []enginebridge.Event {
	return []enginebridge.Event{
		text("m1", "# Heading one\n"),
		text("m1", "Inline `code`, **strong**, *emphasis*, and snake_case_left_alone.\n"),
		text("m1", "> a quoted line that is long enough to need wrapping at sixty columns\n"),
		text("m1", "1. first numbered item that also runs past the narrow width to wrap\n"),
		text("m1", "Ambiguity degrades: **unterminated, `unclosed, 2 * 3 * 4, ***both***.\n"),
		// The separator row counts even though it opens without a pipe.
		text("m1", "| component | owns                                    |\n"),
		text("m1", "|:----------|-----------------------------------------|\n"),
		text("m1", "| transcript| the ordered record of one beam session  |\n"),
		text("m1", " ---------- | ---------- \n"),
		text("m1", "```go\n"),
		text("m1", "func Wrap(s string, w int) []string { return textwidth.Wrap(s, w) } // deliberately long enough to overflow every golden width\n"),
		text("m1", "```\n"),
		text("m1", "Back to prose after the fence.\n"),
		ended(libacp.StopReasonEndTurn),
	}
}

func toolScript() []enginebridge.Event {
	return []enginebridge.Event{
		toolOpen("t1", "Read frame.go", libacp.ToolKindRead, libacp.ToolCallStatusPending),
		toolUpdate("t1", libacp.ToolCallStatusInProgress),
		toolUpdate("t1", libacp.ToolCallStatusCompleted),
		toolOpen("t2", "Edit transcript.go", libacp.ToolKindEdit, libacp.ToolCallStatusInProgress),
		toolUpdate("t2", libacp.ToolCallStatusFailed),
		toolOpen("t3", "Grep a title long enough that a narrow terminal has to truncate it", libacp.ToolKindSearch, libacp.ToolCallStatusInProgress),
	}
}

func missionScript() []enginebridge.Event {
	return []enginebridge.Event{
		text("m1", "Dispatching the unit now.\n"),
		text("m1", "Still streaming while the unit wor"),
		report("scout", "finding", "found three call sites\nall of them in the surfaces layer"),
		ask("scout", "may I widen the search to the kernel packages as well?"),
	}
}

// missionLifeScript walks the whole lifecycle vocabulary: the opening
// transition, a plan revision, and all four terminal states. The last two
// rows drop the agent name and explanation to pin the fallbacks.
func missionLifeScript() []enginebridge.Event {
	return []enginebridge.Event{
		text("m1", "Dispatching the units now.\n"),
		missionStatus("porter", "", enginebridge.MissionStatusOpen, ""),
		missionPlan("porter", 2, "split the migration step now that the schema is known", 2, 1, 1),
		missionStatus("porter", enginebridge.MissionStatusOpen, enginebridge.MissionStatusLanded, "migration applied; 3 tables updated"),
		missionStatus("scout", enginebridge.MissionStatusOpen, enginebridge.MissionStatusDerailed, "the branch was deleted under it"),
		missionStatus("auditor", enginebridge.MissionStatusOpen, enginebridge.MissionStatusStuck, "waiting on a credential nobody has"),
		missionStatus("", enginebridge.MissionStatusOpen, enginebridge.MissionStatusAbandoned, ""),
		missionPlan("", 1, "", 0, 0, 3),
	}
}

func shellScript() []enginebridge.Event {
	return []enginebridge.Event{
		shellRun("go test ./internal/surfaces/beamtui/..."),
		terminal("ok  \x1b[32mbeamtui/frame\x1b[0m\t0.01s\n"),
		terminal("ok  beamtui/textwidth\t0."),
		terminal("02s\n\x1b]0;title\x07partial line without a newline"),
	}
}

func noticeScript() []enginebridge.Event {
	return []enginebridge.Event{
		text("m1", "answering as far as the budget allows"),
		ended(libacp.StopReasonMaxTokens),
		text("m2", "second attempt"),
		enginebridge.TurnFailed{SessionID: sess, Err: errors.New("transport closed")},
		toolOpen("t9", "Search the workspace", libacp.ToolKindSearch, libacp.ToolCallStatusInProgress),
		ended(libacp.StopReasonCancelled),
	}
}

var scenarios = map[string][]enginebridge.Event{
	"stream":      streamScript(),
	"markdown":    markdownScript(),
	"tools":       toolScript(),
	"mission":     missionScript(),
	"missionlife": missionLifeScript(),
	"shell":       shellScript(),
	"notice":      noticeScript(),
}

// TestUnit_ScenarioGoldens pins every scenario at 60/80/120 columns plus the
// ASCII variant at 80, scrollback and live encoded separately.
func TestUnit_ScenarioGoldens(t *testing.T) {
	for name, script := range scenarios {
		for _, w := range goldenWidths {
			gold := fmt.Sprintf("%s_unicode_w%d", name, w)
			t.Run(gold, func(t *testing.T) {
				testkit.Golden(t, gold, testkit.EncodeFrame(commit(apply(script...), w, false)))
			})
		}
		gold := name + "_ascii_w80"
		t.Run(gold, func(t *testing.T) {
			testkit.Golden(t, gold, testkit.EncodeFrame(commit(apply(script...), 80, true)))
		})
	}
}

// TestUnit_StreamSettlesOnNewline pins that a mid-word chunk settles nothing
// and a newline settles the whole line.
func TestUnit_StreamSettlesOnNewline(t *testing.T) {
	tr := apply(text("m1", "Hello, wo"))

	if got := tr.TakeAppends(80, false); len(got) != 0 {
		t.Fatalf("mid-word chunk settled %d lines, want 0: %q", len(got), texts(got))
	}
	if got := texts(tr.Live(80, false, "")); len(got) != 1 || got[0] != "Hello, wo" {
		t.Fatalf("live tail = %q, want [\"Hello, wo\"]", got)
	}
	if !tr.HasOpenWork() {
		t.Fatal("HasOpenWork = false while a message is streaming")
	}

	tr.Apply(text("m1", "rld!\nAnd the next li"))
	if got := texts(tr.TakeAppends(80, false)); len(got) != 1 || got[0] != "Hello, world!" {
		t.Fatalf("settled %q, want [\"Hello, world!\"]", got)
	}
	if got := texts(tr.Live(80, false, "")); len(got) != 1 || got[0] != "And the next li" {
		t.Fatalf("live tail = %q, want the new unterminated line", got)
	}

	// The final unterminated line settles when the message ends.
	tr.Apply(ended(libacp.StopReasonEndTurn))
	if got := texts(tr.TakeAppends(80, false)); len(got) != 1 || got[0] != "And the next li" {
		t.Fatalf("turn end settled %q, want the final unterminated line", got)
	}
	if tr.HasOpenWork() {
		t.Fatal("HasOpenWork = true after the turn ended")
	}
}

// TestUnit_TakeAppendsDrains pins that taken means gone.
func TestUnit_TakeAppendsDrains(t *testing.T) {
	tr := apply(text("m1", "one\ntwo\n"))
	first := tr.TakeAppends(80, false)
	if len(first) != 2 {
		t.Fatalf("first take = %q, want two settled lines", texts(first))
	}
	if second := tr.TakeAppends(80, false); len(second) != 0 {
		t.Fatalf("second take = %q, want nothing", texts(second))
	}
	if got := tr.Live(80, false, ""); len(got) != 0 {
		t.Fatalf("live = %q, want empty (everything settled)", texts(got))
	}
}

// TestUnit_LiveIdleEmpty pins that an idle transcript renders no live region.
func TestUnit_LiveIdleEmpty(t *testing.T) {
	if got := New().Live(80, false, "⠙"); len(got) != 0 {
		t.Fatalf("fresh transcript live = %q, want empty", texts(got))
	}
	tr := apply(user("hi"), text("m1", "done\n"), ended(libacp.StopReasonEndTurn))
	tr.TakeAppends(80, false)
	if got := tr.Live(80, false, "⠙"); len(got) != 0 {
		t.Fatalf("settled transcript live = %q, want empty", texts(got))
	}
	if tr.HasOpenWork() {
		t.Fatal("HasOpenWork = true on an idle transcript")
	}
}

// TestUnit_MessageIDChangeStartsNewMessage pins that a mid-stream id change
// settles the old message's unterminated line rather than absorbing new text.
func TestUnit_MessageIDChangeStartsNewMessage(t *testing.T) {
	tr := apply(text("m1", "first message tail"), text("m2", "second message\n"))
	got := texts(tr.TakeAppends(80, false))
	want := []string{"first message tail", "", "second message"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("settled %q, want %q", got, want)
	}
}

// TestUnit_MissionReportNeverSplicesTheLiveStream pins that a report racing
// a live stream settles as its own card, leaving the live tail untouched.
func TestUnit_MissionReportNeverSplicesTheLiveStream(t *testing.T) {
	tr := apply(text("m1", "still streaming when the report lands"))
	tr.TakeAppends(80, false)

	tr.Apply(report("scout", "finding", "three call sites"))

	settled := texts(tr.TakeAppends(80, false))
	joined := strings.Join(settled, "\n")
	if !strings.Contains(joined, "◆ unit scout reported (finding)") {
		t.Fatalf("report did not settle as its own card:\n%s", joined)
	}
	if strings.Contains(joined, "still streaming") {
		t.Fatalf("the live stream was spliced into the appends:\n%s", joined)
	}
	if got := texts(tr.Live(80, false, "")); len(got) != 1 || got[0] != "still streaming when the report lands" {
		t.Fatalf("live tail = %q, want it untouched by the report", got)
	}
	// The stream is still the same message.
	tr.Apply(text("m1", " and keeps going\n"))
	if got := texts(tr.TakeAppends(80, false)); len(got) != 2 || got[1] != "still streaming when the report lands and keeps going" {
		t.Fatalf("continued line = %q, want the interrupted line completed", got)
	}
}

// TestUnit_MissionLifecycleCardsSettleOnArrival pins that plan and status
// cards settle the instant they land, complete, never joining a message
// streaming beside them.
func TestUnit_MissionLifecycleCardsSettleOnArrival(t *testing.T) {
	tr := apply(text("m1", "still streaming when the unit lands"))
	tr.TakeAppends(80, false)

	tr.Apply(missionPlan("porter", 2, "split the migration step", 2, 1, 1))
	tr.Apply(missionStatus("porter", enginebridge.MissionStatusOpen, enginebridge.MissionStatusLanded, "tests green"))

	settled := strings.Join(texts(tr.TakeAppends(80, false)), "\n")
	for _, want := range []string{
		"◆ unit porter plan rev 2 — split the migration step",
		"2 done · 1 running · 1 pending",
		"◆ unit porter landed — tests green",
	} {
		if !strings.Contains(settled, want) {
			t.Fatalf("missing %q in settled appends:\n%s", want, settled)
		}
	}
	if strings.Contains(settled, "still streaming") {
		t.Fatalf("the live stream was spliced into the appends:\n%s", settled)
	}

	// Nothing of either card lingers in the live region, and the interrupted
	// message is still the same open message.
	if got := texts(tr.Live(80, false, "")); len(got) != 1 || got[0] != "still streaming when the unit lands" {
		t.Fatalf("live tail = %q, want it untouched by the lifecycle cards", got)
	}
	tr.Apply(text("m1", " and keeps going\n"))
	if got := texts(tr.TakeAppends(80, false)); len(got) != 2 || got[1] != "still streaming when the unit lands and keeps going" {
		t.Fatalf("continued line = %q, want the interrupted line completed", got)
	}
}

// TestUnit_MissionStatusStyles pins which style role each lifecycle state
// takes.
func TestUnit_MissionStatusStyles(t *testing.T) {
	cases := []struct {
		status string
		want   frame.StyleID
	}{
		{enginebridge.MissionStatusLanded, frame.StyleDone},
		{enginebridge.MissionStatusDerailed, frame.StyleFailed},
		{enginebridge.MissionStatusStuck, frame.StyleWarn},
		{enginebridge.MissionStatusAbandoned, frame.StyleMuted},
		// Neither claims an outcome: open and unknown status alike.
		{enginebridge.MissionStatusOpen, frame.StyleStrong},
		{"paused", frame.StyleStrong},
	}
	for _, c := range cases {
		tr := apply(missionStatus("porter", enginebridge.MissionStatusOpen, c.status, ""))
		lines := tr.TakeAppends(80, false)
		if len(lines) != 1 {
			t.Fatalf("status %q settled %d lines, want 1: %q", c.status, len(lines), texts(lines))
		}
		head := lines[0][0]
		if head.Style != c.want {
			t.Fatalf("status %q: head span %q has style %q, want %q", c.status, head.Text, head.Style, c.want)
		}
	}
}

// TestUnit_ToolCardLifecycle pins that pending/in-progress update one live
// line in place and a terminal status settles it exactly once.
func TestUnit_ToolCardLifecycle(t *testing.T) {
	tr := apply(toolOpen("t1", "Read frame.go", libacp.ToolKindRead, libacp.ToolCallStatusPending))
	if got := tr.TakeAppends(80, false); len(got) != 0 {
		t.Fatalf("open settled %q, want nothing until a terminal status", texts(got))
	}
	if got := texts(tr.Live(80, false, "⠙")); len(got) != 1 || got[0] != "· Read frame.go" {
		t.Fatalf("pending live card = %q", got)
	}
	if !tr.HasOpenWork() {
		t.Fatal("HasOpenWork = false with an open tool call")
	}

	tr.Apply(toolUpdate("t1", libacp.ToolCallStatusInProgress))
	if got := texts(tr.Live(80, false, "⠙")); len(got) != 1 || got[0] != "⠙ Read frame.go" {
		t.Fatalf("running live card = %q, want the spinner glyph", got)
	}
	if got := tr.TakeAppends(80, false); len(got) != 0 {
		t.Fatalf("transition settled %q, want nothing — cards update in place", texts(got))
	}

	// A patch-shaped update restates neither title nor kind.
	tr.Apply(toolUpdate("t1", libacp.ToolCallStatusCompleted))
	got := texts(tr.TakeAppends(80, false))
	if len(got) != 1 || got[0] != "✓ Read frame.go" {
		t.Fatalf("settled %q, want exactly one completed card line", got)
	}
	if live := tr.Live(80, false, "⠙"); len(live) != 0 {
		t.Fatalf("live = %q after the card settled, want empty", texts(live))
	}
	if tr.HasOpenWork() {
		t.Fatal("HasOpenWork = true after the only tool call completed")
	}

	// Trailing updates (output, locations) must not print the card twice.
	tr.Apply(toolUpdate("t1", libacp.ToolCallStatusCompleted))
	tr.Apply(enginebridge.ToolCallUpdated{SessionID: sess, ToolCallID: "t1", Title: "Read frame.go"})
	if again := tr.TakeAppends(80, false); len(again) != 0 {
		t.Fatalf("late update re-appended %q", texts(again))
	}
}

// TestUnit_ToolCardStatusGlyphs pins the status glyph vocabulary in both variants.
func TestUnit_ToolCardStatusGlyphs(t *testing.T) {
	cases := []struct {
		status         libacp.ToolCallStatus
		unicode, ascii string
	}{
		{libacp.ToolCallStatusCompleted, "✓ t", "+ t"},
		{libacp.ToolCallStatusFailed, "✗ t", "x t"},
	}
	for _, c := range cases {
		for _, ascii := range []bool{false, true} {
			tr := apply(toolOpen("t1", "t", libacp.ToolKindOther, c.status))
			want := c.unicode
			if ascii {
				want = c.ascii
			}
			got := texts(tr.TakeAppends(80, ascii))
			if len(got) != 1 || got[0] != want {
				t.Fatalf("status %q ascii=%v: %q, want [%q]", c.status, ascii, got, want)
			}
		}
	}
}

// TestUnit_OpenToolCallNeverDangles pins that a call the turn ended
// underneath settles as unfinished instead of spinning forever.
func TestUnit_OpenToolCallNeverDangles(t *testing.T) {
	tr := apply(
		toolOpen("t1", "Search the workspace", libacp.ToolKindSearch, libacp.ToolCallStatusInProgress),
		ended(libacp.StopReasonCancelled),
	)
	got := texts(tr.TakeAppends(80, false))
	if len(got) != 3 || got[0] != "· Search the workspace" || got[2] != "— cancelled" {
		t.Fatalf("settled %q, want the abandoned card then the cancel notice", got)
	}
	if live := tr.Live(80, false, "⠙"); len(live) != 0 {
		t.Fatalf("live = %q, want no dangling card", texts(live))
	}
	if tr.HasOpenWork() {
		t.Fatal("HasOpenWork = true after the turn ended under an open call")
	}
}

// TestUnit_CodeFenceIsNeverWrapped pins that a fenced line is one span at
// any width, byte-identical to the source.
func TestUnit_CodeFenceIsNeverWrapped(t *testing.T) {
	src := strings.Repeat("abcdefghij", 20) // 200 columns

	for _, w := range []int{20, 40, 60, 80, 120} {
		t.Run(fmt.Sprint(w), func(t *testing.T) {
			lines := apply(text("m1", "```go\n"), text("m1", src+"\n"), text("m1", "```\n")).TakeAppends(w, false)
			if len(lines) != 3 {
				t.Fatalf("width %d: %d lines, want fence, code, fence", w, len(lines))
			}
			code := lines[1]
			if code.Text() != src {
				t.Fatalf("width %d: code line text = %q, want the source verbatim", w, code.Text())
			}
			if len(code) != 1 || code[0].Style != frame.StyleCode {
				t.Fatalf("width %d: code line = %+v, want a single StyleCode span", w, code)
			}
		})
	}

	// The live tail inside a fence is code too.
	open := apply(text("m1", "```go\n"), text("m1", src))
	open.TakeAppends(40, false)
	live := open.Live(40, false, "")
	if len(live) != 1 || live[0].Text() != src {
		t.Fatalf("live fenced tail = %q, want the source verbatim", texts(live))
	}
}

// TestUnit_InlineMarkdown pins the inline styling table, ambiguity fallbacks
// included.
func TestUnit_InlineMarkdown(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want string
	}{
		{"plain", "just words", "[assistant]just words[/]"},
		{"code", "call `Wrap(s, w)` now", "[assistant]call [/][code]Wrap(s, w)[/][assistant] now[/]"},
		{"strong", "this is **load bearing**", "[assistant]this is [/][strong]load bearing[/]"},
		{"emphasis", "a *soft* word", "[assistant]a [/][em]soft[/][assistant] word[/]"},
		{"mixed", "`a` and **b** and *c*", "[code]a[/][assistant] and [/][strong]b[/][assistant] and [/][em]c[/]"},
		{"heading", "## Plan", "[heading]## Plan[/]"},
		{"heading_needs_space", "#1 priority", "[assistant]#1 priority[/]"},
		{"bullet", "- an item", "[assistant]- an item[/]"},
		{"numbered", "1. an item", "[assistant]1. an item[/]"},
		// The marker is styled, not replaced: a copied quote pastes back as
		// the markdown the agent wrote.
		{"quote", "> quoted", "[border]> [/][muted]quoted[/]"},
		// Ambiguity: degrade to literal source, never garble.
		{"unclosed_code", "an `unclosed span", "[assistant]an `unclosed span[/]"},
		{"unclosed_strong", "**unterminated bold", "[assistant]**unterminated bold[/]"},
		{"arithmetic", "2 * 3 * 4 = 24", "[assistant]2 * 3 * 4 = 24[/]"},
		{"snake_case", "call do_the_thing_now", "[assistant]call do_the_thing_now[/]"},
		{"triple_marker", "***both***", "[assistant]***both***[/]"},
		{"nested_marker", "**a*b**", "[assistant]**a*b**[/]"},
		{"empty_code", "`` ticks", "[assistant]`` ticks[/]"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tr := apply(text("m1", c.src+"\n"))
			got := strings.TrimSuffix(testkit.EncodeLines(tr.TakeAppends(240, false)), "\n")
			if got != c.want {
				t.Fatalf("source %q\n got %s\nwant %s", c.src, got, c.want)
			}
		})
	}
}

// proseSources is one source line in each shape the prose renderer knows.
// want is the text it must settle as (only markdown markers are consumed);
// first and cont are the prefixes the live region hangs wrapped rows off.
var proseSources = []struct{ src, want, first, cont string }{
	{src: "the quick brown fox jumps over the lazy dog while text keeps its spans"},
	// A padded key/value line: the space run outlasts every narrow width.
	{src: "name" + strings.Repeat(" ", 45) + "value"},
	{src: "trailing spaces at the end   "},
	{src: "   leading spaces at the start"},
	{src: "日本語 mixed with ascii and a run of      spaces"},
	{src: "# a heading long enough that every golden width would have wrapped it twice"},
	{src: "> a quoted line long enough that every golden width would have wrapped it", first: "> ", cont: "> "},
	{src: "- a bulleted line long enough that every golden width would have wrapped it", first: "- ", cont: "  "},
	{src: "1. a numbered line long enough that every width would have wrapped it", first: "1. ", cont: "   "},
	{
		src:  "the quick brown fox `code` and **strong** and the lazy dog",
		want: "the quick brown fox code and strong and the lazy dog",
	},
}

// TestUnit_SettledProseIsOneUnwrappedLine pins that one source line settles
// as exactly one line at every width, carrying its source verbatim.
func TestUnit_SettledProseIsOneUnwrappedLine(t *testing.T) {
	for _, c := range proseSources {
		want := c.want
		if want == "" {
			want = c.src
		}
		t.Run(c.src[:min(len(c.src), 24)], func(t *testing.T) {
			for w := 4; w <= 140; w++ {
				lines := apply(text("m1", c.src+"\n")).TakeAppends(w, false)
				if len(lines) != 1 {
					t.Fatalf("width %d: settled %d lines, want one: %q", w, len(lines), texts(lines))
				}
				if got := lines[0].Text(); got != want {
					t.Fatalf("width %d: settled %q, want the source %q", w, got, want)
				}
			}
		})
	}
}

// TestUnit_SettledTranscriptIsWidthIndependent pins the same property over
// whole scripts: settled output is byte-identical at 4 columns and at 140.
// Tool cards are excluded (see TestUnit_CardNeverExceedsWidth).
func TestUnit_SettledTranscriptIsWidthIndependent(t *testing.T) {
	script := []enginebridge.Event{
		user(strings.Repeat("a user turn that keeps going ", 6) + "\nand a second line of it"),
		text("m1", "# "+strings.Repeat("heading ", 12)+"\n"),
		text("m1", strings.Repeat("prose with `code` and **strong** spans ", 6)+"\n"),
		text("m1", "> "+strings.Repeat("quoted ", 20)+"\n"),
		text("m1", "- "+strings.Repeat("bulleted ", 20)+"\n"),
		text("m1", "| cell | "+strings.Repeat("wide cell | ", 12)+"\n"),
		thought("m2", strings.Repeat("thinking ", 20)+"\n"),
		report("a-very-long-agent-name", "finding", strings.Repeat("report body ", 20)),
		terminal(strings.Repeat("shell output ", 20) + "\n"),
	}
	for _, ascii := range []bool{false, true} {
		want := testkit.EncodeLines(apply(script...).TakeAppends(80, ascii))
		for w := 4; w <= 140; w++ {
			if got := testkit.EncodeLines(apply(script...).TakeAppends(w, ascii)); got != want {
				t.Fatalf("ascii=%v width %d differs from width 80:\n got:\n%s\nwant:\n%s", ascii, w, got, want)
			}
		}
	}
}

// TestUnit_LiveWrappingLosesNothing pins that wrapping the live region loses
// no text: joining the wrapped rows back together reproduces the source.
func TestUnit_LiveWrappingLosesNothing(t *testing.T) {
	for _, c := range proseSources {
		want := c.want
		if want == "" {
			want = c.src
		}
		// Prefixes are the only thing wrapping may add.
		body := strings.TrimPrefix(want, c.first)
		for w := 12; w <= 120; w++ {
			// No trailing newline: the line stays in the live region.
			live := apply(text("m1", c.src)).Live(w, false, "")
			var joined strings.Builder
			for i, l := range live {
				row, prefix := l.Text(), c.first
				if i > 0 {
					prefix = c.cont
				}
				if !strings.HasPrefix(row, prefix) {
					t.Fatalf("width %d: live row %d = %q, want the prefix %q", w, i, row, prefix)
				}
				joined.WriteString(strings.TrimPrefix(row, prefix))
				if got := textwidth.Width(row); got > w {
					t.Fatalf("width %d: live row %d is %d cells (%q)", w, i, got, row)
				}
			}
			if got := joined.String(); got != body {
				t.Fatalf("width %d: live rows join to %q, want %q", w, got, body)
			}
		}
	}
}

// TestUnit_LiveWrappingKeepsWhitespaceRuns pins that a padding column wider
// than the wrap width survives rather than being trimmed away.
func TestUnit_LiveWrappingKeepsWhitespaceRuns(t *testing.T) {
	src := "name" + strings.Repeat(" ", 45) + "value"
	joined := strings.Join(texts(apply(text("m1", src)).Live(40, false, "")), "")
	if joined != src {
		t.Fatalf("rows join to %q (%d runes), want the source (%d runes)",
			joined, len([]rune(joined)), len([]rune(src)))
	}
	if n := strings.Count(joined, " "); n != 45 {
		t.Fatalf("%d spaces survived the wrap, want 45", n)
	}
}

// TestUnit_StylesSurviveAWrap pins that an inline span straddling a break
// keeps its style on both rows.
func TestUnit_StylesSurviveAWrap(t *testing.T) {
	src := "leading words **emphasised body text** trailing words"
	for w := 12; w <= 60; w++ {
		var strong []string
		for _, l := range apply(text("m1", src)).Live(w, false, "") {
			for _, s := range l {
				if s.Style == frame.StyleStrong {
					strong = append(strong, s.Text)
				}
			}
		}
		got := strings.ReplaceAll(strings.Join(strong, ""), " ", "")
		if want := "emphasisedbodytext"; got != want {
			t.Fatalf("width %d: strong spans joined to %q, want %q", w, got, want)
		}
	}
}

// TestUnit_MarkdownTableRowsShipVerbatim pins that a table row renders as
// one unstyled span, verbatim, like a fenced code line.
func TestUnit_MarkdownTableRowsShipVerbatim(t *testing.T) {
	rows := []string{
		"| component | owns |",
		"|:----------|------|",
		"| a | b *c* d `e` |",
		"  | indented | row |",
		" ---- | :---: ",
	}
	for _, src := range rows {
		t.Run(src, func(t *testing.T) {
			for _, w := range []int{4, 20, 80} {
				lines := apply(text("m1", src+"\n")).TakeAppends(w, false)
				if len(lines) != 1 {
					t.Fatalf("width %d: %d lines, want one: %q", w, len(lines), texts(lines))
				}
				if got := lines[0].Text(); got != src {
					t.Fatalf("width %d: %q, want the source verbatim", w, got)
				}
				if len(lines[0]) != 1 || lines[0][0].Style != frame.StyleNone {
					t.Fatalf("width %d: %+v, want a single unstyled span", w, lines[0])
				}
			}
		})
	}

	// Prose merely containing a pipe is not a table.
	for _, src := range []string{"run `go test ./... | tee log`", "---", "- a | b"} {
		got := apply(text("m1", src+"\n")).TakeAppends(80, false)
		if len(got) != 1 || got[0][0].Style == frame.StyleNone {
			t.Fatalf("%q rendered as a table row: %+v", src, got)
		}
	}

	// A pipe inside a fenced block is code, not a table.
	lines := apply(text("m1", "```sh\n"), text("m1", "| grep foo\n"), text("m1", "```\n")).TakeAppends(80, false)
	if len(lines) != 3 || lines[1][0].Style != frame.StyleCode {
		t.Fatalf("fenced pipe line = %+v, want StyleCode", lines[1])
	}

	// The live tail of a table row is unwrapped too.
	wide := "| a table row | with columns wider than any terminal beam supports |"
	live := apply(text("m1", wide)).Live(20, false, "")
	if len(live) != 1 || live[0].Text() != wide {
		t.Fatalf("live table row = %q, want the source on one row", texts(live))
	}
}

// TestUnit_UserEchoPrefixAndIndent pins that the sigil carries the styling,
// text stays unstyled, and continuations line up under it.
func TestUnit_UserEchoPrefixAndIndent(t *testing.T) {
	tr := apply(user("first line\nsecond line"))
	got := testkit.EncodeLines(tr.TakeAppends(80, false))
	want := "[user]❯ [/]first line\n  second line\n"
	if got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}

	// A line longer than the terminal is still one line.
	long := strings.Repeat("word ", 12)
	lines := apply(user(long)).TakeAppends(30, false)
	if len(lines) != 1 {
		t.Fatalf("long user line settled as %d lines, want one: %q", len(lines), texts(lines))
	}
	if got, want := lines[0].Text(), "❯ "+long; got != want {
		t.Fatalf("settled %q, want %q", got, want)
	}

	// ASCII variant.
	if got := texts(apply(user("hi")).TakeAppends(80, true)); got[0] != "> hi" {
		t.Fatalf("ascii user line = %q, want \"> hi\"", got[0])
	}
}

// TestUnit_ShellSanitizer pins that escape sequences are stripped, newlines
// settle lines, and a sequence split across chunks is still recognized whole.
func TestUnit_ShellSanitizer(t *testing.T) {
	cases := []struct {
		name   string
		chunks []string
		want   []string
	}{
		{"plain", []string{"hello\n"}, []string{"hello"}},
		{"sgr", []string{"\x1b[32mgreen\x1b[0m\n"}, []string{"green"}},
		{"osc_bel", []string{"\x1b]0;window title\x07after\n"}, []string{"after"}},
		{"osc_st", []string{"\x1b]8;;http://x\x1b\\link\n"}, []string{"link"}},
		{"cursor_motion", []string{"a\x1b[2Kb\x1b[1;1Hc\n"}, []string{"abc"}},
		// CR overwrites the line from column zero; last write wins.
		{"carriage_return", []string{"progress\rdone\n"}, []string{"done"}},
		{"progress_counter", []string{"10%\r20%\r30%\n"}, []string{"30%"}},
		{"progress_across_chunks", []string{"10%", "\r20%\n"}, []string{"20%"}},
		{"crlf_is_one_line_ending", []string{"one\r\ntwo\r\n"}, []string{"one", "two"}},
		{"crlf_split_across_chunks", []string{"one\r", "\ntwo\n"}, []string{"one", "two"}},
		// Tabs expand to 8-column stops.
		{"tab_expands", []string{"a\tb\n"}, []string{"a       b"}},
		{"c0_dropped", []string{"a\x00\x08b\x07\n"}, []string{"ab"}},
		{"utf8_survives", []string{"日本語 ✓\n"}, []string{"日本語 ✓"}},
		{"split_csi", []string{"a\x1b[3", "2mb\n"}, []string{"ab"}},
		{"split_esc", []string{"a\x1b", "[0mb\n"}, []string{"ab"}},
		{"split_osc", []string{"\x1b]0;ti", "tle\x07x\n"}, []string{"x"}},
		{"two_lines", []string{"one\ntwo\n"}, []string{"one", "two"}},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tr := New()
			for _, ch := range c.chunks {
				tr.Apply(terminal(ch))
			}
			got := texts(tr.TakeAppends(80, false))
			if strings.Join(got, "|") != strings.Join(c.want, "|") {
				t.Fatalf("chunks %q settled %q, want %q", c.chunks, got, c.want)
			}
			for _, l := range got {
				if strings.ContainsRune(l, 0x1b) {
					t.Fatalf("escape survived sanitizing: %q", l)
				}
			}
		})
	}
}

// TestUnit_ShellResetReplaces pins that a re-delivered snapshot replaces the
// buffer, announces itself, and settles the displaced unterminated tail first.
func TestUnit_ShellResetReplaces(t *testing.T) {
	tr := apply(terminal("first output\npartial tail"), terminalReset("first output\n"))
	got := texts(tr.TakeAppends(80, false))
	want := []string{"first output", "partial tail", "", "· shell reconnected — replaying", "", "first output"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("settled %q, want %q", got, want)
	}
	if live := tr.Live(80, false, ""); len(live) != 0 {
		t.Fatalf("live = %q, want the tail settled rather than still pending", texts(live))
	}
}

// TestUnit_ShellTailSettlesOnSessionChange pins that a chunk from a
// different session ends the current shell stream and settles its tail.
func TestUnit_ShellTailSettlesOnSessionChange(t *testing.T) {
	tr := apply(
		terminal("kept tail"),
		enginebridge.TerminalChunk{SessionID: other, Chunk: "other session\n"},
	)
	got := texts(tr.TakeAppends(80, false))
	want := []string{"kept tail", "other session"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("settled %q, want %q", got, want)
	}
}

// TestUnit_TurnNotices pins that end_turn is silent, every other reason
// annotates, and a failed turn is unmistakably not assistant prose.
func TestUnit_TurnNotices(t *testing.T) {
	cases := []struct {
		name  string
		ev    enginebridge.Event
		want  []string
		ascii []string
	}{
		{"end_turn", ended(libacp.StopReasonEndTurn), nil, nil},
		{"empty", ended(""), nil, nil},
		{"max_tokens", ended(libacp.StopReasonMaxTokens), []string{"— max tokens"}, []string{"- max tokens"}},
		{"cancelled", ended(libacp.StopReasonCancelled), []string{"— cancelled"}, []string{"- cancelled"}},
		{"refusal", ended(libacp.StopReasonRefusal), []string{"— refusal"}, []string{"- refusal"}},
		{
			"failed",
			enginebridge.TurnFailed{SessionID: sess, Err: errors.New("transport closed")},
			[]string{"✗ turn failed: transport closed"},
			[]string{"x turn failed: transport closed"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := texts(apply(c.ev).TakeAppends(80, false))
			if strings.Join(got, "|") != strings.Join(c.want, "|") {
				t.Fatalf("settled %q, want %q", got, c.want)
			}
			gotASCII := texts(apply(c.ev).TakeAppends(80, true))
			if strings.Join(gotASCII, "|") != strings.Join(c.ascii, "|") {
				t.Fatalf("ascii settled %q, want %q", gotASCII, c.ascii)
			}
		})
	}
}

// TestUnit_SettledLinesNeverChange pins that lines taken at one width are
// byte-identical to a run that never saw a later resize.
func TestUnit_SettledLinesNeverChange(t *testing.T) {
	for name, script := range scenarios {
		t.Run(name, func(t *testing.T) {
			half := len(script) / 2

			resized := New()
			for _, e := range script[:half] {
				resized.Apply(e)
			}
			before := testkit.EncodeLines(resized.TakeAppends(80, false))
			for _, e := range script[half:] {
				resized.Apply(e)
			}
			resized.TakeAppends(120, false) // the width change lands here

			steady := New()
			for _, e := range script[:half] {
				steady.Apply(e)
			}
			if want := testkit.EncodeLines(steady.TakeAppends(80, false)); want != before {
				t.Fatalf("lines settled before the resize differ:\n got:\n%s\nwant:\n%s", before, want)
			}
		})
	}
}

// TestUnit_PlanUpdatedSettlesAsAPlanCard pins that the agent's own plan
// reaches the transcript. Every other ACP client renders it; beam used to
// drop it on the floor, so an agent that announced what it was about to do
// announced it to nobody.
func TestUnit_PlanUpdatedSettlesAsAPlanCard(t *testing.T) {
	tr := apply(enginebridge.PlanUpdated{SessionID: sess, Entries: []libacp.PlanEntry{
		{Content: "read the retry loop", Status: libacp.PlanStatusCompleted},
		{Content: "add exponential backoff", Status: libacp.PlanStatusInProgress},
		{Content: "update the tests", Status: libacp.PlanStatusPending},
		// No status at all: unknown reads as pending, never as done.
		{Content: "write the changelog"},
	}})

	got := texts(tr.TakeAppends(80, false))
	want := []string{
		"◆ plan  1 done · 1 running · 2 pending",
		"  ✓ read the retry loop",
		"  ▸ add exponential backoff",
		"  · update the tests",
		"  · write the changelog",
	}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("plan settled %q, want %q", got, want)
	}

	// The three states differ by glyph, not only by color, in ASCII too.
	tr = apply(enginebridge.PlanUpdated{SessionID: sess, Entries: []libacp.PlanEntry{
		{Content: "done thing", Status: libacp.PlanStatusCompleted},
		{Content: "running thing", Status: libacp.PlanStatusInProgress},
		{Content: "pending thing", Status: libacp.PlanStatusPending},
	}})
	marks := map[string]bool{}
	for _, l := range texts(tr.TakeAppends(80, true))[1:] {
		marks[strings.Fields(l)[0]] = true
	}
	if len(marks) != 3 {
		t.Fatalf("ASCII plan marks collapse to %v — a plan must be readable without color", marks)
	}
}

// TestUnit_IgnoresNonTranscriptEvents pins that events other components own
// leave no mark, even when handed the whole stream.
func TestUnit_IgnoresNonTranscriptEvents(t *testing.T) {
	evs := []enginebridge.Event{
		// A plan is a transcript fact (see above), but an EMPTY one is the
		// agent saying it has none, which is not worth a card.
		enginebridge.PlanUpdated{SessionID: sess},
		enginebridge.UsageUpdated{SessionID: sess, Used: 10, Size: 100},
		enginebridge.CommandsUpdated{SessionID: sess},
		enginebridge.ConfigOptionUpdated{SessionID: sess},
		enginebridge.ModeUpdated{SessionID: sess, ModeID: "code"},
		enginebridge.SessionInfoUpdated{SessionID: sess, Title: "a session"},
		enginebridge.PermissionRequested{SessionID: sess, ToolCallID: "t1", Title: "Write file"},
		enginebridge.ShellRunResult{SessionID: sess, Started: true, Snapshot: "ignored"},
		enginebridge.UnknownUpdate{SessionID: sess, Kind: "future_kind"},
	}
	tr := apply(evs...)
	if got := tr.TakeAppends(80, false); len(got) != 0 {
		t.Fatalf("non-transcript events settled %q", texts(got))
	}
	if got := tr.Live(80, false, "⠙"); len(got) != 0 {
		t.Fatalf("non-transcript events produced live lines %q", texts(got))
	}
	if tr.HasOpenWork() {
		t.Fatal("HasOpenWork = true from non-transcript events")
	}

	// A PermissionRequested does not open a card: approval-cards owns it.
	tr.Apply(toolOpen("t1", "Write file", libacp.ToolKindEdit, libacp.ToolCallStatusCompleted))
	if got := texts(tr.TakeAppends(80, false)); len(got) != 1 {
		t.Fatalf("settled %q, want the one real tool card", got)
	}
}

// TestUnit_CrossSessionEventStartsANewMessage pins that a MessageID
// collision across sessions never splices two agents' prose into one line.
func TestUnit_CrossSessionEventStartsANewMessage(t *testing.T) {
	tr := apply(
		text("m1", "session one tail"),
		enginebridge.TextDelta{SessionID: other, MessageID: "m1", Text: "session two\n"},
	)
	got := texts(tr.TakeAppends(80, false))
	want := []string{"session one tail", "", "session two"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("settled %q, want %q", got, want)
	}
}

// TestUnit_NoticesNeverExceedWidth pins that beam's own turn chrome, unlike
// transcript content, is laid out against the width and must respect it,
// down to width 4 where elision markers stop fitting.
func TestUnit_NoticesNeverExceedWidth(t *testing.T) {
	script := []enginebridge.Event{
		enginebridge.TurnFailed{SessionID: sess, Err: errors.New(strings.Repeat("a long transport failure ", 8))},
		// Short enough to fit the narrowest width; re-delivery is scripted too.
		terminal("ok\n"),
		terminalReset("ok\n"),
		ended(libacp.StopReasonMaxTokens),
	}
	for _, ascii := range []bool{false, true} {
		for w := 4; w <= 140; w++ {
			for i, l := range apply(script...).TakeAppends(w, ascii) {
				if got := textwidth.Width(l.Text()); got > w {
					t.Fatalf("ascii=%v width %d line %d: %d cells (%q)", ascii, w, i, got, l.Text())
				}
			}
		}
	}
}

// TestUnit_CardNeverExceedsWidth pins the same width property for the one
// line that truncates rather than wraps.
func TestUnit_CardNeverExceedsWidth(t *testing.T) {
	titles := []string{
		"Read internal/surfaces/beamtui/comp/transcript/render.go",
		"日本語のとても長いタイトル",
		"x",
		strings.Repeat("long ", 60),
	}
	statuses := []libacp.ToolCallStatus{
		libacp.ToolCallStatusPending,
		libacp.ToolCallStatusInProgress,
		libacp.ToolCallStatusCompleted,
		libacp.ToolCallStatusFailed,
	}
	for _, title := range titles {
		for _, st := range statuses {
			for _, ascii := range []bool{false, true} {
				for w := 4; w <= 140; w++ {
					tr := apply(toolOpen("t1", title, libacp.ToolKindOther, st))
					f := commit(tr, w, ascii)
					for _, l := range append(append([]frame.Line{}, f.Scrollback...), f.Live...) {
						if got := textwidth.Width(l.Text()); got > w {
							t.Fatalf("ascii=%v width %d status %q: %d cells (%q)", ascii, w, st, got, l.Text())
						}
					}
				}
			}
		}
	}
}

// Sanitizing: every event string is untrusted and becomes a frame.Span the
// engine draws as literal cells, so ANSI/C0 and bidi controls must never
// reach one.

// TestUnit_UntrustedTextNeverReachesASpan runs one hostile string through
// every ingest point and asserts none of them leaks a control character.
func TestUnit_UntrustedTextNeverReachesASpan(t *testing.T) {
	const evil = "before\x1b[2Jafter\x1b]0;retitled\x07\ttab\x7f‮flip"

	scripts := map[string][]enginebridge.Event{
		"TextDelta":       {text("m1", evil+"\n")},
		"ThoughtDelta":    {thought("m1", evil+"\n")},
		"UserEcho":        {user(evil)},
		"ToolCallOpened":  {toolOpen("t1", evil, libacp.ToolKindRead, libacp.ToolCallStatusCompleted)},
		"ToolCallUpdated": {enginebridge.ToolCallUpdated{SessionID: sess, ToolCallID: "t1", Title: evil, Status: libacp.ToolCallStatusFailed}},
		"MissionReport":   {report(evil, evil, evil)},
		"MissionAsk":      {ask(evil, evil)},
		// Lifecycle-card strings are just as untrusted as a report's.
		"MissionStatusChanged": {missionStatus(evil, evil, evil, evil)},
		"MissionPlanRevised":   {missionPlan(evil, 3, evil, 1, 1, 1)},
		"ShellRunStarted":      {shellRun(evil)},
		"TerminalChunk":        {terminal(evil + "\n")},
		"TurnFailed":           {enginebridge.TurnFailed{SessionID: sess, Err: errors.New(evil)}},
	}

	for name, script := range scripts {
		t.Run(name, func(t *testing.T) {
			tr := apply(script...)
			f := commit(tr, 200, false)
			for _, l := range append(append([]frame.Line{}, f.Scrollback...), f.Live...) {
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
		})
	}
}

// TestUnit_EscapeInProseIsStrippedNotDrawn pins that a clear-screen sequence
// in agent prose is stripped, never executed, while the surrounding words survive.
func TestUnit_EscapeInProseIsStrippedNotDrawn(t *testing.T) {
	got := texts(apply(text("m1", "before\x1b[2Jafter\n")).TakeAppends(80, false))
	if len(got) != 1 || got[0] != "beforeafter" {
		t.Fatalf("settled %q, want [\"beforeafter\"]", got)
	}
}

// Closed-message hygiene.

// TestUnit_LateDeltaAfterTurnEndIsDropped pins that a chunk arriving after
// the turn-end notice is dropped rather than reopening the message.
func TestUnit_LateDeltaAfterTurnEndIsDropped(t *testing.T) {
	tr := apply(text("m1", "the answer\n"), ended(libacp.StopReasonCancelled))
	tr.TakeAppends(80, false)

	tr.Apply(text("m1", "a straggler chunk\n"))
	if got := texts(tr.TakeAppends(80, false)); len(got) != 0 {
		t.Fatalf("late delta settled %q, want nothing", got)
	}
	if got := tr.Live(80, false, "⠙"); len(got) != 0 {
		t.Fatalf("late delta produced live lines %q", texts(got))
	}
	if tr.HasOpenWork() {
		t.Fatal("HasOpenWork = true after a late delta for an ended message")
	}

	// A genuinely new message is unaffected.
	tr.Apply(text("m2", "the next turn\n"))
	if got := texts(tr.TakeAppends(80, false)); len(got) != 2 || got[1] != "the next turn" {
		t.Fatalf("new message after the close settled %q", got)
	}
}

// TestUnit_UnidentifiedMessageReopensAfterTurnEnd pins that an id-less
// message key reopens after a turn ends, since every assistant message in a
// process shares one such key on the real wire.
func TestUnit_UnidentifiedMessageReopensAfterTurnEnd(t *testing.T) {
	tr := apply(
		text("", "first reply\n"),
		ended(libacp.StopReasonEndTurn),
		text("", "second reply\n"),
		ended(libacp.StopReasonEndTurn),
	)
	got := texts(tr.TakeAppends(80, false))
	want := []string{"first reply", "", "second reply"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("settled %q, want %q — an id-less second turn must still render", got, want)
	}

	// The same for thoughts, equally id-less, and a cancel instead of end_turn.
	tr = apply(
		thought("", "thinking once\n"),
		ended(libacp.StopReasonCancelled),
		thought("", "thinking twice\n"),
	)
	got = texts(tr.TakeAppends(80, false))
	want = []string{"thinking once", "", "— cancelled", "", "thinking twice"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("settled %q, want %q", got, want)
	}
}

// TestUnit_UnidentifiedThoughtAndTextAlternate pins that an agent
// interleaving reasoning and prose within one turn loses nothing, even
// though each switch changes the stream key.
func TestUnit_UnidentifiedThoughtAndTextAlternate(t *testing.T) {
	tr := apply(
		thought("", "let me look\n"),
		text("", "here is what I found\n"),
		thought("", "one more check\n"),
		text("", "and here is the answer\n"),
		ended(libacp.StopReasonEndTurn),
	)
	got := texts(tr.TakeAppends(80, false))
	want := []string{
		"let me look", "", "here is what I found", "",
		"one more check", "", "and here is the answer",
	}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("settled %q, want %q", got, want)
	}
}

// TestUnit_ReplayedHistorySettlesItsLastMessage pins that EndReplay settles
// the last replayed message, which a replay's missing TurnEnded would
// otherwise leave stuck in the live region forever.
func TestUnit_ReplayedHistorySettlesItsLastMessage(t *testing.T) {
	tr := apply(
		user("first question"),
		text("", "first answer"),
		user("second question"),
		text("", "second answer"),
	)

	// The UserEcho rule alone already settles everything but the tail.
	got := texts(tr.TakeAppends(80, false))
	want := []string{"❯ first question", "", "first answer", "", "❯ second question"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("mid-replay settled %q, want %q", got, want)
	}
	if live := texts(tr.Live(80, false, "")); len(live) != 1 || live[0] != "second answer" {
		t.Fatalf("live = %q, want the last replayed message still open", live)
	}

	tr.EndReplay()
	if got := texts(tr.TakeAppends(80, false)); len(got) != 2 || got[1] != "second answer" {
		t.Fatalf("EndReplay settled %q, want the last replayed message", got)
	}
	if live := tr.Live(80, false, ""); len(live) != 0 {
		t.Fatalf("live = %q after EndReplay, want empty", texts(live))
	}
	if tr.HasOpenWork() {
		t.Fatal("HasOpenWork = true after the replay ended")
	}

	// A live turn after the replay is unaffected.
	tr.Apply(text("", "a live answer\n"))
	if got := texts(tr.TakeAppends(80, false)); len(got) != 2 || got[1] != "a live answer" {
		t.Fatalf("post-replay turn settled %q, want it rendered", got)
	}
}

// TestUnit_EndReplayLeavesNothingDangling pins that a replay ending under an
// open tool call settles it as abandoned rather than leaving it spinning.
func TestUnit_EndReplayLeavesNothingDangling(t *testing.T) {
	tr := apply(toolOpen("t1", "Read frame.go", libacp.ToolKindRead, libacp.ToolCallStatusInProgress))
	tr.EndReplay()
	if got := texts(tr.TakeAppends(80, false)); len(got) != 1 || got[0] != "· Read frame.go" {
		t.Fatalf("settled %q, want the abandoned card", got)
	}
	if tr.HasOpenWork() {
		t.Fatal("HasOpenWork = true after EndReplay")
	}
}

// TestUnit_EveryTurnOfAnUnidentifiedFixtureRenders pins the same id-less
// guarantee against the shared real-wire fixtures rather than hand-built
// scripts.
func TestUnit_EveryTurnOfAnUnidentifiedFixtureRenders(t *testing.T) {
	cases := []struct {
		name   string
		script []enginebridge.Event
		end    bool // the fixture is a history replay: settle it explicitly
		want   []string
	}{
		{
			name:   "TwoTurnsEmptyMessageID",
			script: testkit.FixtureTwoTurnsEmptyMessageID(sess),
			want: []string{
				"It retries on 429 and on any 5xx from the upstream API.",
				"Timeouts are retried too, with the same backoff.",
			},
		},
		{
			name:   "StreamingTurnEmptyMessageID",
			script: testkit.FixtureStreamingTurnEmptyMessageID(sess),
			want: []string{
				"I'll add exponential backoff to the retry loop now.",
				"Backoff is now capped at 30s with jitter.",
			},
		},
		{
			name:   "ReplayedHistory",
			script: testkit.FixtureReplayedHistory(sess),
			end:    true,
			want: []string{
				"It pulls batches off the queue and writes them through the store.",
				"In the write path, with a fixed 5s delay.",
			},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tr := apply(c.script...)
			if c.end {
				tr.EndReplay()
			}
			settled := strings.Join(texts(tr.TakeAppends(200, false)), "\n")
			for i, want := range c.want {
				if !strings.Contains(settled, want) {
					t.Fatalf("turn %d's prose never settled — %q is missing from:\n%s", i+1, want, settled)
				}
			}
			if live := tr.Live(200, false, ""); len(live) != 0 {
				t.Fatalf("live = %q, want everything settled", texts(live))
			}
		})
	}
}

// TestUnit_EmptyDeltaIsInvisible pins that a zero-length chunk is not a
// message and never pins HasOpenWork true on its own.
func TestUnit_EmptyDeltaIsInvisible(t *testing.T) {
	tr := apply(text("m1", ""))
	if tr.HasOpenWork() {
		t.Fatal("an empty TextDelta opened a message")
	}
	if got := tr.TakeAppends(80, false); len(got) != 0 {
		t.Fatalf("empty delta settled %q", texts(got))
	}
	if got := tr.Live(80, false, "⠙"); len(got) != 0 {
		t.Fatalf("empty delta produced live lines %q", texts(got))
	}

	// It must not close a stream that is open either.
	tr = apply(text("m1", "streaming tail"), text("m2", ""))
	if got := texts(tr.Live(80, false, "")); len(got) != 1 || got[0] != "streaming tail" {
		t.Fatalf("live = %q, want the open stream untouched by an empty delta", got)
	}
	if got := tr.TakeAppends(80, false); len(got) != 0 {
		t.Fatalf("an empty delta closed the open message: settled %q", texts(got))
	}
}

// Separators.

// TestUnit_InterleavedStreamsDoNotChurnSeparators pins that an assistant
// message and a shell command running at once interleave their flushes with
// no separator, since neither group has ended.
func TestUnit_InterleavedStreamsDoNotChurnSeparators(t *testing.T) {
	tr := apply(
		text("m1", "assistant one\n"),
		terminal("shell one\n"),
		text("m1", "assistant two\n"),
		terminal("shell two\n"),
		text("m1", "assistant three\n"),
	)
	got := texts(tr.TakeAppends(80, false))
	want := []string{"assistant one", "shell one", "assistant two", "shell two", "assistant three"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("settled %q, want %q with no separators", got, want)
	}

	// Once the message has ended, the next group gets its separator.
	tr.Apply(ended(libacp.StopReasonEndTurn))
	tr.Apply(report("scout", "finding", "a finding"))
	got = texts(tr.TakeAppends(80, false))
	if len(got) == 0 || got[0] != "" {
		t.Fatalf("after the turn ended, the report settled as %q, want a leading blank", got)
	}
}

// TestUnit_DegenerateWidths pins that a width narrower than any glyph
// degrades gracefully rather than panicking.
func TestUnit_DegenerateWidths(t *testing.T) {
	for name, script := range scenarios {
		for _, w := range []int{-1, 0, 1, 2, 3, 5} {
			for _, ascii := range []bool{false, true} {
				tr := apply(script...)
				f := commit(tr, w, ascii)
				for _, l := range append(append([]frame.Line{}, f.Scrollback...), f.Live...) {
					for _, s := range l {
						if strings.ContainsRune(s.Text, '\n') {
							t.Fatalf("%s w=%d ascii=%v: newline inside a span %q", name, w, ascii, s.Text)
						}
					}
				}
			}
		}
	}
}

// TestUnit_UsesOnlyClosedStyleIDs pins that the component never invents a
// StyleID outside frame's closed set.
func TestUnit_UsesOnlyClosedStyleIDs(t *testing.T) {
	known := map[frame.StyleID]bool{}
	for _, id := range frame.All() {
		known[id] = true
	}
	for name, script := range scenarios {
		for _, ascii := range []bool{false, true} {
			for _, w := range append(goldenWidths, 20) {
				tr := apply(script...)
				f := commit(tr, w, ascii)
				for _, l := range append(append([]frame.Line{}, f.Scrollback...), f.Live...) {
					for _, s := range l {
						if !known[s.Style] {
							t.Fatalf("%s ascii=%v w=%d: span %q uses unknown StyleID %q", name, ascii, w, s.Text, s.Style)
						}
					}
				}
			}
		}
	}
}

// TestUnit_ASCIIVariantIsPureASCII pins that no unicode glyph leaks into the
// ASCII variant (agent-authored content is exempt).
func TestUnit_ASCIIVariantIsPureASCII(t *testing.T) {
	script := []enginebridge.Event{
		user("plain"),
		text("m1", "> quoted\n"),
		report("scout", "finding", "body"),
		ask("scout", "question"),
		shellRun("ls"),
		terminalReset("out\n"),
		toolOpen("t1", "ok", libacp.ToolKindRead, libacp.ToolCallStatusCompleted),
		toolOpen("t2", "bad", libacp.ToolKindRead, libacp.ToolCallStatusFailed),
		toolOpen("t3", "open", libacp.ToolKindRead, libacp.ToolCallStatusInProgress),
		ended(libacp.StopReasonMaxTokens),
		enginebridge.TurnFailed{SessionID: sess, Err: errors.New("boom")},
	}
	tr := apply(script...)
	f := commit(tr, 80, true)
	for _, l := range append(append([]frame.Line{}, f.Scrollback...), f.Live...) {
		for _, r := range l.Text() {
			if r > 0x7f {
				t.Fatalf("ASCII variant emitted %q in %q", r, l.Text())
			}
		}
	}
}

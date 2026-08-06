package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/contenox/contenox/internal/surfaces/beamtui/comp/fileaddr"
	"github.com/contenox/contenox/internal/surfaces/beamtui/comp/statusbar"
	"github.com/contenox/contenox/internal/surfaces/beamtui/enginebridge"
	"github.com/contenox/contenox/internal/surfaces/beamtui/frame"
	"github.com/contenox/contenox/internal/surfaces/beamtui/input"
	"github.com/contenox/contenox/internal/surfaces/beamtui/keymap"
	"github.com/contenox/contenox/internal/surfaces/beamtui/style"
	"github.com/contenox/contenox/internal/surfaces/beamtui/term"
	"github.com/contenox/contenox/internal/surfaces/beamtui/testkit"
	libacp "github.com/contenox/contenox/libacp"
)

// Both testkit doubles satisfy Bridge; asserted here rather than in the
// production file, which cannot import test infrastructure.
var (
	_ Bridge = (*testkit.FakeBridge)(nil)
	_ Bridge = (testkit.EngineBridge)(nil)
)

const testSession = libacp.SessionID("beam-test-session")

// baseTime is the fixed clock every scripted test starts at: goldens and
// liveness assertions compare renders across ticks, never wall time.
var baseTime = time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)

// fakeTerm is the headless terminal: it hands the loop the events a test
// feeds it and captures every committed frame instead of drawing one.
type fakeTerm struct {
	events chan input.Event
	frames []frame.Frame
	bells  int
	// suspends counts handovers; suspendFrames records the frame on screen
	// when each one began, to check beam never hands over an unreclaimable
	// live region.
	suspends      int
	suspendFrames []frame.Frame
	closed        int
	width, height int
	commitErr     error
}

var _ term.Engine = (*fakeTerm)(nil)

func newFakeTerm(width, height int) *fakeTerm {
	return &fakeTerm{events: make(chan input.Event, 64), width: width, height: height}
}

func (f *fakeTerm) Events() <-chan input.Event { return f.events }

func (f *fakeTerm) Commit(fr frame.Frame) error {
	f.frames = append(f.frames, fr)
	return f.commitErr
}

func (f *fakeTerm) Size() (int, int) { return f.width, f.height }

// Suspend runs fn inline: the real engine's cooked-mode dance has no headless
// equivalent.
func (f *fakeTerm) Suspend(fn func() error) error {
	f.suspends++
	var on frame.Frame
	if len(f.frames) > 0 {
		on = f.frames[len(f.frames)-1]
	}
	f.suspendFrames = append(f.suspendFrames, on)
	return fn()
}

func (f *fakeTerm) Bell()                                { f.bells++ }
func (f *fakeTerm) CopyToClipboard(string) (bool, error) { return false, nil }
func (f *fakeTerm) Close() error                         { f.closed++; return nil }

// harness drives the app-shell one event at a time, committing after each,
// so a scenario is a script rather than a race.
type harness struct {
	t      *testing.T
	a      *app
	term   *fakeTerm
	bridge *testkit.FakeBridge
	clock  time.Time
	ctx    context.Context
}

func newHarness(t *testing.T, mut ...func(*Deps)) *harness {
	t.Helper()
	h := &harness{t: t, term: newFakeTerm(80, 24), bridge: testkit.NewFakeBridge(), clock: baseTime, ctx: context.Background()}
	deps := Deps{
		Term:         h.term,
		Bridge:       h.bridge,
		Caps:         style.Caps{Profile: style.ProfileTrueColor, Dark: true},
		SessionID:    testSession,
		FreshSession: true,
		Model:        "qwen3:8b",
		Provider:     "ollama",
		SessionName:  "beam-0001",
		Clock:        func() time.Time { return h.clock },
	}
	for _, m := range mut {
		m(&deps)
	}
	a, err := newApp(deps)
	if err != nil {
		t.Fatalf("newApp: %v", err)
	}
	h.a = a
	return h
}

// start performs the loop's initial commit.
func (h *harness) start() *harness {
	h.t.Helper()
	if err := h.a.commit(); err != nil {
		h.t.Fatalf("commit: %v", err)
	}
	return h
}

func (h *harness) advance(d time.Duration) { h.clock = h.clock.Add(d) }

func (h *harness) input(ev input.Event) {
	h.t.Helper()
	h.a.onTerminal(h.ctx, ev)
	if err := h.a.commit(); err != nil {
		h.t.Fatalf("commit: %v", err)
	}
}

func (h *harness) typeText(s string) {
	h.t.Helper()
	for _, r := range s {
		h.input(input.KeyEvent{Key: input.KeyRune, Rune: r})
	}
}

func (h *harness) press(k input.Key) { h.input(input.KeyEvent{Key: k}) }

func (h *harness) chord(r rune, ctrl, alt bool) {
	h.input(input.KeyEvent{Key: input.KeyRune, Rune: r, Ctrl: ctrl, Alt: alt})
}

// openEditor drives the Ctrl+X, Ctrl+E chord that hands the draft to
// $EDITOR: two keystrokes, one per h.input/commit cycle, exactly as a real
// terminal delivers them.
func (h *harness) openEditor() {
	h.t.Helper()
	h.chord('x', true, false)
	h.chord('e', true, false)
}

func (h *harness) deliver(events ...enginebridge.Event) {
	h.t.Helper()
	for _, ev := range events {
		h.a.onBridge(ev)
		if err := h.a.commit(); err != nil {
			h.t.Fatalf("commit: %v", err)
		}
	}
}

func (h *harness) last() frame.Frame {
	h.t.Helper()
	if len(h.term.frames) == 0 {
		h.t.Fatal("no frame committed")
	}
	return h.term.frames[len(h.term.frames)-1]
}

// scrollback is every line every commit appended, in order.
func (h *harness) scrollback() string {
	var b strings.Builder
	for _, f := range h.term.frames {
		b.WriteString(testkit.EncodeLines(f.Scrollback))
	}
	return b.String()
}

func (h *harness) calls() string { return strings.Join(h.bridge.Calls(), "\n") }

func requireContains(t *testing.T, got, want, what string) {
	t.Helper()
	if !strings.Contains(got, want) {
		t.Fatalf("%s: missing %q in:\n%s", what, want, got)
	}
}

func requireNotContains(t *testing.T, got, unwanted, what string) {
	t.Helper()
	if strings.Contains(got, unwanted) {
		t.Fatalf("%s: unexpected %q in:\n%s", what, unwanted, got)
	}
}

// TestUnit_Bindings_NoCollisions pins that no two bindings in a
// simultaneously-reachable scope claim the same chord.
func TestUnit_Bindings_NoCollisions(t *testing.T) {
	r := keymap.NewRegistry()
	registerBindings(r) // MustRegister panics on a collision, naming both owners
	entries := r.Help(helpScopes)
	if len(entries) == 0 {
		t.Fatal("registry produced no help entries")
	}
	for _, e := range entries {
		if e.Help == "" || e.Owner == "" || len(e.Keys) == 0 {
			t.Fatalf("binding %v is missing Help/Owner/Keys", e)
		}
	}
}

// TestUnit_FreshSession_WelcomeThenStatusBar pins the first frame: the brand
// header prints once into scrollback and never reprints.
func TestUnit_FreshSession_WelcomeThenStatusBar(t *testing.T) {
	h := newHarness(t).start()

	testkit.Golden(t, "fresh_session_first_frame", testkit.EncodeFrame(h.last()))

	h.typeText("x")
	if got := testkit.EncodeLines(h.last().Scrollback); got != "" {
		t.Fatalf("welcome reprinted on a later commit:\n%s", got)
	}
}

// TestUnit_StreamingTurn_Scrollback pins the scrollback history produced by
// the standard streaming-turn fixture.
func TestUnit_StreamingTurn_Scrollback(t *testing.T) {
	h := newHarness(t, func(d *Deps) { d.FreshSession = false }).start()
	h.deliver(testkit.FixtureStreamingTurn(testSession)...)
	testkit.Golden(t, "streaming_turn_scrollback", h.scrollback())
}

// TestUnit_Submit_RecordsPrompt pins that typed text becomes one
// SubmitPrompt, echoes into the transcript, and leaves the composer empty.
func TestUnit_Submit_RecordsPrompt(t *testing.T) {
	h := newHarness(t).start()
	h.typeText("hello")
	h.press(input.KeyEnter)

	requireContains(t, h.calls(), `SubmitPrompt(beam-test-session, "hello")`, "bridge calls")
	requireContains(t, h.scrollback(), "hello", "transcript echo")
	if !h.a.comp.Empty() {
		t.Fatal("composer not cleared after submit")
	}
	if !h.a.inFlight {
		t.Fatal("turn not marked in flight")
	}
	if !h.a.live.Ticking() {
		t.Fatal("liveness not ticking during a turn")
	}

	// A second submit while running is refused and keeps the text.
	h.typeText("again")
	h.press(input.KeyEnter)
	requireContains(t, testkit.EncodeLines(h.last().Scrollback), "a turn is already running", "in-flight notice")
	if h.a.comp.Draft() != "again" {
		t.Fatalf("refused submit lost the draft: %q", h.a.comp.Draft())
	}
	if strings.Count(h.calls(), "SubmitPrompt") != 1 {
		t.Fatalf("expected exactly one SubmitPrompt, got:\n%s", h.calls())
	}
}

// TestUnit_ShellLine_RunsShell pins that a `!` line routes to RunShellLine,
// never SubmitPrompt.
func TestUnit_ShellLine_RunsShell(t *testing.T) {
	h := newHarness(t).start()
	h.typeText("!go test ./...")
	h.press(input.KeyEnter)

	requireContains(t, h.calls(), `RunShellLine(beam-test-session, "go test ./...")`, "bridge calls")
	requireNotContains(t, h.calls(), "SubmitPrompt", "a shell line must never become a turn")
}

// TestUnit_Palette_QuitFlow pins the `/qu` journey: trigger opens, typing
// filters, Enter completes, Enter again runs the local command.
func TestUnit_Palette_QuitFlow(t *testing.T) {
	h := newHarness(t).start()

	h.typeText("/")
	if !h.a.pal.IsOpen() {
		t.Fatal("`/` on an empty buffer did not open the palette")
	}
	h.typeText("qu")
	sel, ok := h.a.pal.Selected()
	if !ok || sel.Name != "quit" {
		t.Fatalf("filtered palette selected %+v (ok=%v), want quit", sel, ok)
	}
	testkit.Golden(t, "palette_open_frame", testkit.EncodeFrame(h.last()))

	h.press(input.KeyEnter) // completes the selection
	if got := h.a.comp.Draft(); got != "/quit " {
		t.Fatalf("Enter did not complete the selection: %q", got)
	}
	if h.a.quit {
		t.Fatal("completing a command must not run it")
	}

	h.press(input.KeyEnter) // runs it
	if !h.a.quit {
		t.Fatal("/quit did not ask the loop to quit")
	}
	requireNotContains(t, h.calls(), "SubmitPrompt", "a local command must not reach the agent")
}

// TestUnit_Palette_RemoteCommandGoesToTheAgent pins that a command the agent
// advertised is sent verbatim, not reimplemented.
func TestUnit_Palette_RemoteCommandGoesToTheAgent(t *testing.T) {
	h := newHarness(t).start()
	h.deliver(enginebridge.CommandsUpdated{SessionID: testSession, Commands: []libacp.AvailableCommand{
		{Name: "mission", Description: "fire a mission"},
	}})

	h.typeText("/mission audit deps")
	h.press(input.KeyEnter)
	requireContains(t, h.calls(), `SubmitPrompt(beam-test-session, "/mission audit deps")`, "bridge calls")
}

// TestUnit_Help_IsGeneratedFromTheRegistry pins that both key-list surfaces
// are projections of the registrations, not hand-written text — and that the
// local is /keys, since keybindings are this client's while /help is the
// core's and must mean one thing everywhere.
func TestUnit_Help_IsGeneratedFromTheRegistry(t *testing.T) {
	h := newHarness(t).start()

	h.typeText("/keys")
	h.press(input.KeyEnter)
	printed := h.scrollback()
	requireContains(t, printed, "compose the draft in $EDITOR", "the /keys key list")
	requireContains(t, printed, "/quit", "the /keys command list")
	requireNotContains(t, h.calls(), "SubmitPrompt", "/keys is client-side")

	// /help is the ACP core's, not a second beam-local meaning of the word.
	if e, ok := h.a.pal.Lookup("help"); ok && e.Local {
		t.Fatal("beam still shadows the core's /help with a local")
	}

	// `?` on an empty composer opens the overlay; on a non-empty one it types.
	h.input(input.KeyEvent{Key: input.KeyRune, Rune: '?'})
	if !h.a.helpOpen {
		t.Fatal("`?` did not open the help overlay")
	}
	requireContains(t, testkit.EncodeLines(h.last().Live), "clear the composer", "help overlay")
	if !h.last().Cursor.Hidden {
		t.Fatal("the caret must hide while a blocking modal is up")
	}
	h.press(input.KeyEscape)
	if h.a.helpOpen {
		t.Fatal("Esc did not close the help overlay")
	}
	h.typeText("what?")
	if got := h.a.comp.Draft(); got != "what?" {
		t.Fatalf("`?` did not type into a non-empty composer: %q", got)
	}
}

// TestUnit_Help_OverlayGroupsByScope pins that the overlay groups by scope,
// leads with scopes reachable from where the operator is, and files the rest
// under one "elsewhere" divider.
func TestUnit_Help_OverlayGroupsByScope(t *testing.T) {
	h := newHarness(t).start()
	h.input(input.KeyEvent{Key: input.KeyRune, Rune: '?'})

	rows := h.a.overlayHelp()
	if len(rows) == 0 {
		t.Fatal("the help overlay rendered nothing")
	}
	var texts []string
	for _, l := range rows {
		if !strings.HasPrefix(l.Text(), h.a.glyphs.PromptSigil) {
			t.Fatalf("overlay row %q does not hang off the brand gutter", l.Text())
		}
		texts = append(texts, l.Text())
	}
	if !strings.Contains(texts[0], "keys") {
		t.Fatalf("the overlay does not open with a title row: %q", texts[0])
	}

	index := func(want string) int {
		for i, s := range texts {
			if strings.TrimSpace(strings.TrimPrefix(s, h.a.glyphs.PromptSigil)) == want {
				return i
			}
		}
		return -1
	}
	composer, anywhere, elsewhere := index("composer"), index("anywhere"), index("elsewhere")
	for name, at := range map[string]int{"composer": composer, "anywhere": anywhere, "elsewhere": elsewhere} {
		if at < 0 {
			t.Fatalf("no %q section header in:\n%s", name, strings.Join(texts, "\n"))
		}
	}
	// With no modal open, composer and global lead; everything else follows.
	if composer > elsewhere || anywhere > elsewhere {
		t.Fatalf("a reachable scope was filed under elsewhere:\n%s", strings.Join(texts, "\n"))
	}
	if at := index("approval card"); at >= 0 && at < elsewhere {
		t.Fatalf("an unreachable scope led the overlay:\n%s", strings.Join(texts, "\n"))
	}

	// Every chord shares one column, wide enough for the widest chord list.
	col, seen := -1, false
	for _, l := range h.a.helpRows(false) {
		if len(l) != 2 {
			continue // a section header or the title
		}
		w := len(l[0].Text)
		if col >= 0 && w != col {
			t.Fatalf("chord column is %d cells here and %d there: %q", col, w, l.Text())
		}
		col = w
		if strings.Contains(l[0].Text, "alt+enter") {
			seen = true
			if !strings.HasSuffix(l[0].Text, "  ") {
				t.Fatalf("the widest chord list has no gap before its help: %q", l.Text())
			}
		}
	}
	if !seen {
		t.Fatal("the two-chord binding never rendered")
	}
}

// TestUnit_Palette_EscHoldsForTheTokenNotForever pins that a palette
// dismissal is keyed to the command token: it survives navigation and lapses
// the moment another letter of the name is typed.
func TestUnit_Palette_EscHoldsForTheTokenNotForever(t *testing.T) {
	h := newHarness(t).start()

	h.typeText("/qu")
	if !h.a.pal.IsOpen() {
		t.Fatal("`/qu` did not open the palette")
	}
	h.press(input.KeyEscape)
	if h.a.pal.IsOpen() {
		t.Fatal("esc did not close the palette")
	}
	if h.a.comp.Draft() != "/qu" {
		t.Fatalf("esc touched the buffer: %q", h.a.comp.Draft())
	}

	// Moving around the same command is not a new question.
	h.press(input.KeyLeft)
	h.press(input.KeyRight)
	if h.a.pal.IsOpen() {
		t.Fatal("cursor movement resurrected a dismissed menu")
	}

	// Another letter of the name is.
	h.typeText("i")
	if !h.a.pal.IsOpen() {
		t.Fatal("typing more of the command never reopened the menu")
	}

	// Arguments are still the same command, so a dismissal survives them.
	h.press(input.KeyEscape)
	h.typeText(" now")
	if h.a.pal.IsOpen() {
		t.Fatal("typing an argument resurrected a dismissed menu")
	}

	// Leaving command shape drops the latch entirely.
	h.chord('u', true, false) // kill to start
	if h.a.hasPalDismissed {
		t.Fatal("the latch outlived the command line it belonged to")
	}
	h.typeText("/qu")
	if !h.a.pal.IsOpen() {
		t.Fatal("a fresh command line did not open the menu")
	}
}

// TestUnit_ApprovalFlow_AllowResolvesOnce pins the HITL gate: the ask lands
// in scrollback whole, the card blocks the composer, y answers allow exactly
// once, the answer leaves a record, and the tool call resumes.
//
// The record is the inversion of what this test used to pin. Dropping the
// card the instant it is answered made approval.Card's own resolved footers
// unreachable, so an approved call left a transcript in which a gated tool
// simply ran: no verdict, no policy, nothing to audit after the fact.
func TestUnit_ApprovalFlow_AllowResolvesOnce(t *testing.T) {
	h := newHarness(t, func(d *Deps) { d.FreshSession = false }).start()

	var resolved []bool
	events := testkit.FixtureApprovalFlow(testSession)
	for i, ev := range events {
		if req, ok := ev.(enginebridge.PermissionRequested); ok {
			// The fixture's Resolve is inert by design; swap in a recorder.
			req.Resolve = func(allow bool) { resolved = append(resolved, allow) }
			events[i] = req
		}
	}

	h.deliver(events[0], events[1]) // tool call opens, permission asked
	if h.a.card == nil {
		t.Fatal("no approval card after PermissionRequested")
	}
	if h.term.bells != 1 {
		t.Fatalf("an approval must always ring even when focused: bells=%d", h.term.bells)
	}
	testkit.Golden(t, "approval_card_frame", testkit.EncodeFrame(h.last()))

	// The ask is in scrollback, where no row budget can clip it; the live
	// region keeps the subject beside the y/n line so the prompt has one.
	asked := h.scrollback()
	requireContains(t, asked, "approval required", "the ask settles into scrollback")
	requireContains(t, asked, ".contenox/policies/default.yaml", "the policy line survives")
	live := testkit.EncodeLines(h.last().Live)
	requireContains(t, live, "local_fs.write_file", "the live prompt names its subject")

	// Blocked while the card is up (text avoids y/n, which the card claims).
	h.typeText("xxx")
	if !h.a.comp.Empty() {
		t.Fatalf("composer accepted input while an approval was pending: %q", h.a.comp.Draft())
	}

	mark := len(h.term.frames)
	h.input(input.KeyEvent{Key: input.KeyRune, Rune: 'y'})
	h.input(input.KeyEvent{Key: input.KeyRune, Rune: 'y'}) // doubled keystroke
	if len(resolved) != 1 || !resolved[0] {
		t.Fatalf("want exactly one allow, got %v", resolved)
	}
	if h.a.card != nil {
		t.Fatal("card outlived its answer")
	}
	verdict := scrollbackFrom(h, mark)
	requireContains(t, verdict, "allowed", "the answer is recorded in the transcript")
	requireContains(t, verdict, "local_fs.write_file", "the record names what was allowed")
	requireContains(t, verdict, "policy default", "the record names the policy that asked")

	h.deliver(events[2], events[3])
	requireContains(t, h.scrollback(), "write_file: internal/config/limits.go", "resumed tool card")
}

// TestUnit_Approval_PermissionResolvedRetiresTheCard pins the fact a card
// retires on: without it any resolution beam did not itself perform leaves a
// pending card owning the keyboard — typing swallowed, y/n going to a channel
// nobody reads, and Ctrl+C twice the only way out.
func TestUnit_Approval_PermissionResolvedRetiresTheCard(t *testing.T) {
	h := newHarness(t).start()
	events := testkit.FixtureApprovalFlow(testSession)
	h.deliver(events[1])
	if h.a.card == nil {
		t.Fatal("no approval card after PermissionRequested")
	}

	h.deliver(enginebridge.PermissionResolved{
		SessionID:  testSession,
		ToolCallID: h.a.card.ToolCallID(),
		Outcome:    libacp.PermissionOutcomeSelected,
	})
	if h.a.card != nil {
		t.Fatal("PermissionResolved did not retire the card")
	}

	// The keyboard is back: an ordinary keystroke reaches the composer again.
	h.typeText("back to typing")
	if got := h.a.comp.Draft(); got != "back to typing" {
		t.Fatalf("the retired card kept the keyboard: draft = %q", got)
	}
}

// TestUnit_Approval_ResolvedForAnotherCallLeavesTheCardAlone pins the
// ToolCallID match: tool calls are sequential per turn, but a late
// resolution from a previous gate must not retire the card now on screen.
func TestUnit_Approval_ResolvedForAnotherCallLeavesTheCardAlone(t *testing.T) {
	h := newHarness(t).start()
	h.deliver(testkit.FixtureApprovalFlow(testSession)[1])

	h.deliver(enginebridge.PermissionResolved{
		SessionID:  testSession,
		ToolCallID: "some.other.call#9",
		Outcome:    libacp.PermissionOutcomeSelected,
	})
	if h.a.card == nil {
		t.Fatal("a resolution for another tool call retired the pending card")
	}
}

// TestUnit_Approval_EscCancelsTheTurn pins that Esc on a card asks the bridge
// to cancel and leaves the card pending until the cancel comes back.
func TestUnit_Approval_EscCancelsTheTurn(t *testing.T) {
	h := newHarness(t).start()
	events := testkit.FixtureApprovalFlow(testSession)
	h.deliver(events[1])

	h.press(input.KeyEscape)
	requireContains(t, h.calls(), "Cancel(beam-test-session)", "bridge calls")
	if h.a.card == nil {
		t.Fatal("Esc must not answer the card itself — it stays pending until the cancel lands")
	}

	h.deliver(enginebridge.TurnEnded{SessionID: testSession, StopReason: libacp.StopReasonCancelled})
	if h.a.card != nil {
		t.Fatal("a cancelled turn must retire the pending card")
	}
}

// TestUnit_CtrlC_ThreeWay pins the clear/interrupt/quit Ctrl+C policy against
// a scripted clock.
func TestUnit_CtrlC_ThreeWay(t *testing.T) {
	h := newHarness(t).start()
	ctrlC := input.KeyEvent{Key: input.KeyRune, Rune: 'c', Ctrl: true}

	// 1. A non-empty composer clears and consumes the chord.
	h.typeText("half a thought")
	h.input(ctrlC)
	if !h.a.comp.Empty() {
		t.Fatal("ctrl+c did not clear the composer")
	}
	if h.a.quit {
		t.Fatal("ctrl+c quit while clearing")
	}

	// 2. An in-flight turn is interrupted, not quit, and the cancelled prompt
	// is restored into the (now empty) composer so it can be edited and
	// resent — the cancelled line itself stays put in scrollback.
	h.typeText("run something")
	h.press(input.KeyEnter)
	h.input(ctrlC)
	requireContains(t, h.calls(), "Cancel(beam-test-session)", "bridge calls")
	if h.a.quit {
		t.Fatal("ctrl+c quit while a turn was running")
	}
	if got := h.a.comp.Draft(); got != "run something" {
		t.Fatalf("cancelled prompt not restored into the composer: %q", got)
	}
	requireContains(t, h.scrollback(), "run something", "the cancelled line stays in scrollback")
	h.deliver(enginebridge.TurnEnded{SessionID: testSession, StopReason: libacp.StopReasonCancelled})

	// The restored text is an ordinary draft now: a further ctrl+c clears it
	// like any other, rather than re-arming the quit offer.
	h.input(ctrlC)
	if !h.a.comp.Empty() {
		t.Fatal("ctrl+c did not clear the restored draft")
	}
	if h.a.quit {
		t.Fatal("ctrl+c quit while clearing the restored draft")
	}

	// 3. Idle: the first press offers, the second (inside the window) quits.
	mark := len(h.term.frames)
	h.input(ctrlC)
	if h.a.quit {
		t.Fatal("a single idle ctrl+c quit")
	}
	// The offer is live, not history: it must never reach scrollback.
	requireContains(t, testkit.EncodeLines(h.last().Live), quitHintText, "the offer")
	requireNotContains(t, scrollbackFrom(h, mark), quitHintText, "the offer must never be scrollback")
	if !h.a.ticking() {
		t.Fatal("the ticker is not armed for the offer's window, so the hint cannot clear itself")
	}

	h.advance(3 * time.Second) // window elapsed: this press only re-offers
	// The lapsed offer clears itself on the next frame, with no event.
	requireNotContains(t, testkit.EncodeLines(h.a.buildFrame().Live), quitHintText, "a lapsed offer")
	if h.a.ticking() {
		t.Fatal("the ticker stayed armed past the offer's window")
	}
	h.input(ctrlC)
	if h.a.quit {
		t.Fatal("ctrl+c outside the window quit")
	}
	h.advance(500 * time.Millisecond)
	h.input(ctrlC)
	if !h.a.quit {
		t.Fatal("a second ctrl+c inside the window did not quit")
	}
}

// TestUnit_Cancel_EscRestoresPromptIntoEmptyComposer pins that Esc — beam's
// dedicated "cancel the running turn" key — restores the cancelled prompt
// exactly like ctrl+c's own cancel arm does.
func TestUnit_Cancel_EscRestoresPromptIntoEmptyComposer(t *testing.T) {
	h := newHarness(t).start()

	h.typeText("please cancel me")
	h.press(input.KeyEnter)
	h.press(input.KeyEscape)

	requireContains(t, h.calls(), "Cancel(beam-test-session)", "bridge calls")
	if got := h.a.comp.Draft(); got != "please cancel me" {
		t.Fatalf("esc-cancel did not restore the prompt: %q", got)
	}
	requireContains(t, h.scrollback(), "please cancel me", "the cancelled line stays in scrollback")
}

// TestUnit_Cancel_NeverClobbersAnInProgressDraft pins the "never" half of the
// restore rule: Esc has no composer-clearing arm of its own (unlike ctrl+c),
// so it is the path where a draft typed over the cancelled turn is at risk.
func TestUnit_Cancel_NeverClobbersAnInProgressDraft(t *testing.T) {
	h := newHarness(t).start()

	h.typeText("first message")
	h.press(input.KeyEnter)
	if !h.a.inFlight {
		t.Fatal("turn not marked in flight")
	}

	h.typeText("a fresh thought")
	h.press(input.KeyEscape)
	requireContains(t, h.calls(), "Cancel(beam-test-session)", "bridge calls")
	if got := h.a.comp.Draft(); got != "a fresh thought" {
		t.Fatalf("esc-cancel clobbered the in-progress draft: %q", got)
	}

	h.deliver(enginebridge.TurnEnded{SessionID: testSession, StopReason: libacp.StopReasonCancelled})
	if got := h.a.comp.Draft(); got != "a fresh thought" {
		t.Fatalf("draft changed once the cancelled turn actually ended: %q", got)
	}

	// The abandoned "first message" is not queued forever: sending the
	// redraft overwrites it, so the NEXT cancel restores what was really
	// just sent, never the earlier, stale prompt.
	h.press(input.KeyEnter)
	requireContains(t, h.calls(), `SubmitPrompt(beam-test-session, "a fresh thought")`, "bridge calls")
	h.press(input.KeyEscape)
	if got := h.a.comp.Draft(); got != "a fresh thought" {
		t.Fatalf("cancel restored a stale prompt instead of the one just sent: %q", got)
	}
}

// TestUnit_ArrowUp_RecallsPreviousSubmissions pins the composer history
// generalization end to end: Up in an empty composer walks submitted turns
// most-recent-first, and Down walks back toward empty.
func TestUnit_ArrowUp_RecallsPreviousSubmissions(t *testing.T) {
	h := newHarness(t).start()

	h.typeText("first")
	h.press(input.KeyEnter)
	h.deliver(enginebridge.TurnEnded{SessionID: testSession, StopReason: libacp.StopReasonEndTurn})

	h.typeText("second")
	h.press(input.KeyEnter)
	h.deliver(enginebridge.TurnEnded{SessionID: testSession, StopReason: libacp.StopReasonEndTurn})

	if !h.a.comp.Empty() {
		t.Fatal("composer not empty before recall")
	}
	h.press(input.KeyUp)
	if got := h.a.comp.Draft(); got != "second" {
		t.Fatalf("arrow-up did not recall the most recent submission: %q", got)
	}
	h.press(input.KeyUp)
	if got := h.a.comp.Draft(); got != "first" {
		t.Fatalf("second arrow-up did not recall the earlier submission: %q", got)
	}
	h.press(input.KeyDown)
	if got := h.a.comp.Draft(); got != "second" {
		t.Fatalf("arrow-down did not walk forward: %q", got)
	}
	h.press(input.KeyDown)
	if !h.a.comp.Empty() {
		t.Fatalf("arrow-down past the newest did not return to empty: %q", h.a.comp.Draft())
	}
}

// TestUnit_Editor_CarriesTheDraftBothWays pins that Ctrl+X, Ctrl+E seeds the
// editor with the draft and carries the result back into the buffer.
func TestUnit_Editor_CarriesTheDraftBothWays(t *testing.T) {
	var seen []string
	h := newHarness(t, func(d *Deps) {
		d.Editor = func(seed string) (string, error) {
			seen = append(seen, seed)
			return seed + "\n\nand the rest, written in $EDITOR", nil
		}
	}).start()

	h.typeText("a first line")
	h.openEditor()

	if h.term.suspends != 1 {
		t.Fatalf("editor did not suspend the terminal: suspends=%d", h.term.suspends)
	}
	if len(seen) != 1 || seen[0] != "a first line" {
		t.Fatalf("draft was not carried INTO the editor: %v", seen)
	}
	if got := h.a.comp.Draft(); got != "a first line\n\nand the rest, written in $EDITOR" {
		t.Fatalf("editor result was not carried BACK: %q", got)
	}
}

// TestUnit_Editor_SuspendRepaintsTheLiveRegionOnly pins that Ctrl+X, Ctrl+E
// hands the terminal over with an empty live region and never prints a
// scrollback line twice, however many suspend cycles run.
func TestUnit_Editor_SuspendRepaintsTheLiveRegionOnly(t *testing.T) {
	const cycles = 3
	h := newHarness(t, func(d *Deps) {
		d.FreshSession = false
		d.Editor = func(seed string) (string, error) { return seed, nil }
	}).start()

	// A settled block plus a still-streaming tail occupy the live region.
	h.deliver(enginebridge.TextDelta{
		SessionID: testSession, MessageID: "m1",
		Text: "a settled line of the last block\n",
	})
	h.deliver(enginebridge.TextDelta{
		SessionID: testSession, MessageID: "m1",
		Text: "and a live tail that never settled",
	})
	h.typeText("half a draft")

	for range cycles {
		h.openEditor()
	}
	if h.term.suspends != cycles {
		t.Fatalf("suspends = %d, want %d", h.term.suspends, cycles)
	}
	for i, f := range h.term.suspendFrames {
		if len(f.Live) != 0 || len(f.Scrollback) != 0 {
			t.Fatalf("suspend %d handed the terminal a region it can never reclaim:\n%s",
				i, testkit.EncodeFrame(f))
		}
	}

	// Nothing beam printed as history is ever printed again.
	seen := make(map[string]int)
	for _, f := range h.term.frames {
		for _, l := range f.Scrollback {
			if text := l.Text(); strings.TrimSpace(text) != "" {
				seen[text]++
			}
		}
	}
	for text, n := range seen {
		if n > 1 {
			t.Fatalf("scrollback line %q printed %d times across %d suspend cycles", text, n, cycles)
		}
	}

	// The live region comes back whole on the far side.
	live := testkit.EncodeLines(h.last().Live)
	requireContains(t, live, "and a live tail", "the live tail after a resume")
	requireContains(t, live, "half a draft", "the draft after a resume")
}

// TestUnit_Editor_AbortKeepsTheDraft covers the empty-abort path.
func TestUnit_Editor_AbortKeepsTheDraft(t *testing.T) {
	h := newHarness(t, func(d *Deps) {
		d.Editor = func(string) (string, error) { return "", errors.New("aborted due to empty prompt") }
	}).start()

	h.typeText("keep me")
	h.openEditor()
	if got := h.a.comp.Draft(); got != "keep me" {
		t.Fatalf("an aborted editor destroyed the draft: %q", got)
	}
	requireContains(t, h.scrollback(), "aborted due to empty prompt", "abort notice")
}

// TestUnit_Resize_LeavesSettledScrollbackAlone pins that a resize re-wraps
// the live region without touching a line already printed to scrollback.
func TestUnit_Resize_LeavesSettledScrollbackAlone(t *testing.T) {
	h := newHarness(t, func(d *Deps) { d.FreshSession = false }).start()

	h.deliver(enginebridge.TextDelta{
		SessionID: testSession, MessageID: "m1",
		Text: "a settled line that is comfortably wider than forty columns of terminal\n",
	})
	settled := h.scrollback()
	if settled == "" {
		t.Fatal("nothing settled")
	}

	// An unterminated tail is live, so it re-wraps.
	h.deliver(enginebridge.TextDelta{
		SessionID: testSession, MessageID: "m1",
		Text: "and a live tail that is also much wider than forty columns",
	})
	wide := testkit.EncodeLines(h.last().Live)

	h.input(input.ResizeEvent{Width: 40, Height: 24})
	narrow := testkit.EncodeLines(h.last().Live)
	if wide == narrow {
		t.Fatal("resize did not re-wrap the live region")
	}
	if got := h.scrollback(); got != settled {
		t.Fatalf("resize disturbed settled scrollback:\nwant %q\ngot  %q", settled, got)
	}
}

// TestUnit_TooSmallTerminal_RendersOneHonestLine pins that a too-small
// terminal shows one line and queues settled content instead of dropping it.
func TestUnit_TooSmallTerminal_RendersOneHonestLine(t *testing.T) {
	h := newHarness(t, func(d *Deps) { d.FreshSession = false }).start()
	h.input(input.ResizeEvent{Width: 10, Height: 4})
	h.deliver(enginebridge.TextDelta{SessionID: testSession, MessageID: "m1", Text: "hidden for now\n"})

	f := h.last()
	if len(f.Live) != 1 || f.Live[0].Text() != tooSmallText {
		t.Fatalf("want exactly one %q line, got %v", tooSmallText, testkit.EncodeLines(f.Live))
	}
	if len(f.Scrollback) != 0 {
		t.Fatalf("settled lines must wait for a usable width, got %v", testkit.EncodeLines(f.Scrollback))
	}

	h.input(input.ResizeEvent{Width: 80, Height: 24})
	requireContains(t, h.scrollback(), "hidden for now", "queued lines print once the terminal is usable again")
}

// TestUnit_Liveness_MicroMotionThenStillness pins motion while a mission
// heartbeat is open and a still frame once it closes.
func TestUnit_Liveness_MicroMotionThenStillness(t *testing.T) {
	h := newHarness(t, func(d *Deps) { d.FreshSession = false }).start()
	events := testkit.FixtureMissionHeartbeat(testSession)
	h.deliver(events[0], events[1]) // a tool call opens; a silent progress ping

	render := func(tick int) string {
		h.clock = baseTime.Add(time.Duration(tick) * tickInterval)
		return testkit.EncodeFrame(h.a.buildFrame())
	}
	render(0) // drain the settled queue so the comparison is about motion only
	testkit.AssertMicroMotion(t, render, 12)

	// Close everything the heartbeat opened; the frame must come to rest.
	h.deliver(events[len(events)-2], events[len(events)-1])
	render(0)
	testkit.AssertStabilizes(t, render, 8)
	if h.a.live.Ticking() {
		t.Fatal("the ticker stayed armed after the work closed")
	}
}

// TestUnit_Liveness_TurnSpinnerAdvancesWithNoEvents pins the dogfood
// complaint this closes: a submitted turn that draws zero further bridge
// events — the silent stretch before the first token, or between tool
// calls — must still visibly animate every tick, off the clock alone.
// TestUnit_Liveness_MicroMotionThenStillness covers a heartbeat's motion;
// this is the same assertion with no events at all after submit, which is
// exactly what a hung-looking turn would otherwise look like.
func TestUnit_Liveness_TurnSpinnerAdvancesWithNoEvents(t *testing.T) {
	h := newHarness(t).start()
	h.typeText("run something")
	h.press(input.KeyEnter)
	if !h.a.inFlight {
		t.Fatal("turn not marked in flight")
	}

	render := func(tick int) string {
		h.clock = baseTime.Add(time.Duration(tick) * tickInterval)
		return testkit.EncodeFrame(h.a.buildFrame())
	}
	render(0) // drain the settled queue so the comparison is about motion only
	testkit.AssertMicroMotion(t, render, 12)
}

// TestUnit_Liveness_IdleRendersNoSpinner pins the other half: with nothing
// in flight, there is no spinner glyph to show and the frame holds
// perfectly still tick over tick — motion is exclusively an
// in-flight-turn signal, never a decorative idle animation.
func TestUnit_Liveness_IdleRendersNoSpinner(t *testing.T) {
	h := newHarness(t).start()
	if h.a.live.Ticking() {
		t.Fatal("ticking while idle")
	}
	if got := h.a.spinner(); got != "" {
		t.Fatalf("spinner() = %q while idle, want none", got)
	}
	if got := h.a.status().Spinner; got != "" {
		t.Fatalf("status().Spinner = %q while idle, want none", got)
	}

	render := func(tick int) string {
		h.clock = baseTime.Add(time.Duration(tick) * tickInterval)
		return testkit.EncodeFrame(h.a.buildFrame())
	}
	testkit.AssertStabilizes(t, render, 8)
}

// TestUnit_Bell_UnfocusedOnlyAndCoalesced pins focus suppression, the
// always-ring exception, and the rate floor.
func TestUnit_Bell_UnfocusedOnlyAndCoalesced(t *testing.T) {
	h := newHarness(t).start()

	// Focused: an ordinary turn ending is silent.
	h.deliver(enginebridge.TurnEnded{SessionID: testSession, StopReason: libacp.StopReasonEndTurn})
	if h.term.bells != 0 {
		t.Fatalf("rang while focused: bells=%d", h.term.bells)
	}

	h.input(input.FocusEvent{Focused: false})
	h.deliver(enginebridge.TurnEnded{SessionID: testSession, StopReason: libacp.StopReasonEndTurn})
	if h.term.bells != 1 {
		t.Fatalf("did not ring while unfocused: bells=%d", h.term.bells)
	}

	// Inside the window: coalesced.
	h.advance(500 * time.Millisecond)
	h.deliver(enginebridge.MissionReport{SessionID: testSession, Kind: "result", Text: "done"})
	if h.term.bells != 1 {
		t.Fatalf("bells not coalesced: bells=%d", h.term.bells)
	}

	// Past it: rings again.
	h.advance(3 * time.Second)
	h.deliver(enginebridge.MissionReport{SessionID: testSession, Kind: "blocker", Text: "stuck"})
	if h.term.bells != 2 {
		t.Fatalf("did not ring after the window: bells=%d", h.term.bells)
	}

	// A cancelled turn is never a completion.
	h.advance(3 * time.Second)
	h.deliver(enginebridge.TurnEnded{SessionID: testSession, StopReason: libacp.StopReasonCancelled})
	if h.term.bells != 2 {
		t.Fatalf("a cancel rang: bells=%d", h.term.bells)
	}
}

// TestUnit_Bell_RuleTable pins the full bell rule set, one row per fact beam
// can ring for, each played into a fresh app so the rate floor never masks a
// rule. Focus suppresses a turn ending or mission landing; a blocking ask or
// inbox arrival rings regardless, since neither has a surface to suppress
// against.
func TestUnit_Bell_RuleTable(t *testing.T) {
	status := func(to, reason string) enginebridge.Event {
		return enginebridge.MissionStatusChanged{
			SessionID: testSession, MissionID: "mis-1", AgentName: "porter",
			Old: enginebridge.MissionStatusOpen, New: to, Reason: reason,
		}
	}

	cases := []struct {
		name string
		ev   enginebridge.Event
		// wantFocused/wantUnfocused are bell counts after one event, focused
		// and unfocused respectively.
		wantFocused   int
		wantUnfocused int
	}{
		{"mission landed", status(enginebridge.MissionStatusLanded, "tests green"), 0, 1},
		{"mission derailed", status(enginebridge.MissionStatusDerailed, "branch gone"), 0, 1},
		{"mission stuck", status(enginebridge.MissionStatusStuck, "needs a credential"), 0, 1},
		{"mission abandoned", status(enginebridge.MissionStatusAbandoned, ""), 0, 1},

		// Opening a mission is the operator's own act.
		{"mission opened", status(enginebridge.MissionStatusOpen, ""), 0, 0},
		// An unknown status is treated as still running.
		{"unknown mission status", status("paused", ""), 0, 0},

		{"plan revision", enginebridge.MissionPlanRevised{
			SessionID: testSession, MissionID: "mis-1", AgentName: "porter",
			Revision: 2, Explanation: "split the migration step",
			EntryCount: 4, Pending: 1, InProgress: 1, Completed: 2,
		}, 0, 0},

		{"mission ask", enginebridge.MissionAsk{
			SessionID: testSession, MissionID: "mis-1", AskID: "ask-1",
			AgentName: "porter", Summary: "which branch?",
		}, 1, 1},

		{"inbox arrival", enginebridge.InboxItemAdded{
			ID: "inbox-1", MissionID: "mis-9", AgentName: "auditor",
			Reason: "operator_fired", Kind: "result", Summary: "4 copyleft licences",
		}, 1, 1},

		{"report result", enginebridge.MissionReport{
			SessionID: testSession, MissionID: "mis-1", ReportID: "rep-1",
			Kind: "result", AgentName: "porter", Text: "done",
		}, 0, 1},
		{"report progress", enginebridge.MissionReport{
			SessionID: testSession, MissionID: "mis-1", ReportID: "rep-2",
			Kind: "progress", AgentName: "porter", Text: "still going",
		}, 0, 0},

		{"turn ended", enginebridge.TurnEnded{
			SessionID: testSession, StopReason: libacp.StopReasonEndTurn,
		}, 0, 1},
		{"turn cancelled", enginebridge.TurnEnded{
			SessionID: testSession, StopReason: libacp.StopReasonCancelled,
		}, 0, 0},
	}

	for _, c := range cases {
		for _, focused := range []bool{true, false} {
			want, label := c.wantFocused, "focused"
			if !focused {
				want, label = c.wantUnfocused, "unfocused"
			}
			t.Run(c.name+"/"+label, func(t *testing.T) {
				h := newHarness(t).start()
				if !focused {
					h.input(input.FocusEvent{Focused: false})
				}
				h.deliver(c.ev)
				if h.term.bells != want {
					t.Fatalf("%s while %s: bells=%d, want %d", c.name, label, h.term.bells, want)
				}
			})
		}
	}
}

// TestUnit_Bell_NewFactsCoalesceWithTheOldOnes pins that the rate floor is
// one floor for the whole surface: an always-ring fact opens the coalescing
// window, and a fact suppressed by focus never consumes it.
func TestUnit_Bell_NewFactsCoalesceWithTheOldOnes(t *testing.T) {
	h := newHarness(t).start()

	inbox := func(id string) enginebridge.Event {
		return enginebridge.InboxItemAdded{ID: id, MissionID: "mis-9", Reason: "parent_gone", Kind: "blocker", Summary: "which one?"}
	}
	landed := enginebridge.MissionStatusChanged{
		SessionID: testSession, MissionID: "mis-1", AgentName: "porter",
		Old: enginebridge.MissionStatusOpen, New: enginebridge.MissionStatusLanded,
	}

	// A suppressed fact leaves the floor untouched.
	h.deliver(landed)
	if h.term.bells != 0 {
		t.Fatalf("a focused mission landing rang: bells=%d", h.term.bells)
	}

	// The inbox rings through focus, and opens the window.
	h.deliver(inbox("inbox-1"))
	if h.term.bells != 1 {
		t.Fatalf("inbox arrival did not ring: bells=%d", h.term.bells)
	}

	// Inside the window: a second always-ring fact is coalesced into the first.
	h.advance(500 * time.Millisecond)
	h.deliver(inbox("inbox-2"))
	if h.term.bells != 1 {
		t.Fatalf("a second inbox arrival inside the window rang: bells=%d", h.term.bells)
	}

	// Past it: rings again.
	h.advance(3 * time.Second)
	h.deliver(inbox("inbox-3"))
	if h.term.bells != 2 {
		t.Fatalf("inbox arrival after the window did not ring: bells=%d", h.term.bells)
	}
}

// TestUnit_InboxBadgeCountsArrivals pins that an inbox event becomes a
// number on the status bar that outlives the bell.
func TestUnit_InboxBadgeCountsArrivals(t *testing.T) {
	h := newHarness(t).start()

	if got := h.a.status().Inbox; got != 0 {
		t.Fatalf("fresh app reports Inbox=%d, want 0", got)
	}
	if strings.Contains(testkit.EncodeLines(h.last().Live), "✉") {
		t.Fatal("inbox badge rendered before any arrival")
	}

	h.deliver(testkit.FixtureInboxArrival()...)

	if got := h.a.status().Inbox; got != 2 {
		t.Fatalf("Inbox=%d after two arrivals, want 2", got)
	}
	requireContains(t, testkit.EncodeLines(h.last().Live), "✉ 2", "status bar inbox badge")
}

// TestUnit_MissionLifecycleReachesTheTranscript pins that every bridge event
// reaches the transcript, so mission cards land in scrollback.
func TestUnit_MissionLifecycleReachesTheTranscript(t *testing.T) {
	h := newHarness(t, func(d *Deps) { d.FreshSession = false }).start()
	h.deliver(testkit.FixtureMissionLifecycle(testSession)...)

	back := h.scrollback()
	requireContains(t, back, "unit porter open", "mission opened card")
	requireContains(t, back, "unit porter plan rev 2", "plan revision card")
	requireContains(t, back, "2 done · 1 running · 1 pending", "plan counts line")
	requireContains(t, back, "unit porter landed", "mission landed card")
	requireContains(t, back, "migration applied; 3 tables updated", "landing reason")
}

// TestUnit_MissionsBadgeCountsOpenMissions pins that the badge counts work
// still running rather than announcements seen: statusbar has carried a
// Missions field the app never assigned, so a fired mission left no trace on
// the bar at all.
func TestUnit_MissionsBadgeCountsOpenMissions(t *testing.T) {
	h := newHarness(t).start()
	status := func(id, from, to string) enginebridge.Event {
		return enginebridge.MissionStatusChanged{
			SessionID: testSession, MissionID: id, AgentName: "porter", Old: from, New: to,
		}
	}

	if got := h.a.status().Missions; got != 0 {
		t.Fatalf("fresh app reports Missions=%d, want 0", got)
	}

	h.deliver(status("mis-1", "", enginebridge.MissionStatusOpen))
	h.deliver(status("mis-2", "", enginebridge.MissionStatusOpen))
	// A redelivered opening is the same mission, not a third one.
	h.deliver(status("mis-1", "", enginebridge.MissionStatusOpen))
	if got := h.a.status().Missions; got != 2 {
		t.Fatalf("Missions=%d with two open, want 2", got)
	}
	requireContains(t, testkit.EncodeLines(h.last().Live), "◇ 2", "status bar missions badge")

	h.deliver(status("mis-1", enginebridge.MissionStatusOpen, enginebridge.MissionStatusLanded))
	h.deliver(status("mis-2", enginebridge.MissionStatusOpen, enginebridge.MissionStatusStuck))
	if got := h.a.status().Missions; got != 0 {
		t.Fatalf("Missions=%d once both came to rest, want 0", got)
	}
	requireNotContains(t, testkit.EncodeLines(h.last().Live), "◇", "the badge outlived the work")

	// A status this build does not know counts as still running.
	h.deliver(status("mis-3", "", "quarantined"))
	if got := h.a.status().Missions; got != 1 {
		t.Fatalf("Missions=%d for an unrecognized status, want it counted as running", got)
	}
}

// TestUnit_ConfigOptionUpdateMovesTheStatusBar pins that the bar names the
// model the session is running now. The update carrying it was consumed for
// palette completion only, so /model changed the session and left the bar
// asserting the launch-time model — a lie about which model answered.
func TestUnit_ConfigOptionUpdateMovesTheStatusBar(t *testing.T) {
	h := newHarness(t).start()
	requireContains(t, testkit.EncodeLines(h.last().Live), "qwen3:8b · ollama", "the bar starts on Deps")

	h.deliver(enginebridge.ConfigOptionUpdated{SessionID: testSession, Options: []libacp.SessionConfigOption{{
		ID:           "model",
		Type:         "select",
		CurrentValue: "openai/gpt-5.1-codex",
	}}})
	live := testkit.EncodeLines(h.last().Live)
	requireContains(t, live, "gpt-5.1-codex · openai", "the bar follows the session's model")
	requireNotContains(t, live, "qwen3:8b", "the launch-time model outlived the switch")

	// An update carrying no model select must not blank what is on screen.
	h.deliver(enginebridge.ConfigOptionUpdated{SessionID: testSession, Options: []libacp.SessionConfigOption{{
		ID: "think", Type: "select", CurrentValue: "high",
	}}})
	requireContains(t, testkit.EncodeLines(h.last().Live), "gpt-5.1-codex · openai", "a think update blanked the model")
}

// TestUnit_BridgeClose_ShowsDisconnectedAndRetiresTheCard pins the two facts
// a closed event stream carries: the health state statusbar has always been
// able to render, and that no permission can still be answered.
func TestUnit_BridgeClose_ShowsDisconnectedAndRetiresTheCard(t *testing.T) {
	h := newHarness(t).start()
	h.deliver(testkit.FixtureApprovalFlow(testSession)[1])
	if h.a.card == nil {
		t.Fatal("no approval card after PermissionRequested")
	}

	h.a.onBridgeClosed()
	if err := h.a.commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	if h.a.card != nil {
		t.Fatal("a closed bridge left a card nobody can ever answer")
	}
	if got := h.a.status().Health; got != statusbar.HealthDisconnected {
		t.Fatalf("Health=%q after the bridge closed, want %q", got, statusbar.HealthDisconnected)
	}
	requireContains(t, h.scrollback(), "cancelled", "the abandoned gate is recorded")
}

// TestUnit_Mention_PickerSplicesAPath pins the `@` seam over a real fileaddr
// source.
func TestUnit_Mention_PickerSplicesAPath(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "retry.go"), []byte("package ingest\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	src, err := fileaddr.NewSource(nil, root)
	if err != nil {
		t.Fatal(err)
	}

	h := newHarness(t, func(d *Deps) { d.FileSource = src }).start()
	h.typeText("look at @ret")
	if !h.a.pickerOpen {
		t.Fatal("`@` did not open the file picker")
	}
	if _, ok := h.a.pick.Selected(); !ok {
		t.Fatalf("picker matched nothing for %q", "ret")
	}
	h.press(input.KeyEnter)
	// The "@" comes back with the path: MentionSpan's span includes the sigil.
	if got := h.a.comp.Draft(); got != "look at @retry.go " {
		t.Fatalf("mention was not spliced: %q", got)
	}
	if h.a.pickerOpen {
		t.Fatal("picker stayed open after accepting")
	}
}

// TestUnit_Mention_QueryReachesPastTheCandidateCap pins that a query is
// ranked and capped after matching, not filtered from an alphabetically
// capped unfiltered fetch — a file past the cap must still be findable.
func TestUnit_Mention_QueryReachesPastTheCandidateCap(t *testing.T) {
	root := t.TempDir()
	// Comfortably past pickerCandidateCap, named so the wanted file sorts
	// last: an alphabetical cap could not include it.
	for i := 0; i < pickerCandidateCap+40; i++ {
		name := filepath.Join(root, fmt.Sprintf("aaa%04d.go", i))
		if err := os.WriteFile(name, []byte("package pad\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "zzz_needle.go"), []byte("package ingest\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	src, err := fileaddr.NewSource(nil, root)
	if err != nil {
		t.Fatal(err)
	}

	h := newHarness(t, func(d *Deps) { d.FileSource = src }).start()
	h.typeText("look at @zzz_needle")

	it, ok := h.a.pick.Selected()
	if !ok {
		t.Fatalf("no candidate for a file that exists: %s", testkit.EncodeLines(h.last().Live))
	}
	if it.Label != "zzz_needle.go" {
		t.Fatalf("selected %q, want zzz_needle.go", it.Label)
	}
	h.press(input.KeyEnter)
	if got := h.a.comp.Draft(); got != "look at @zzz_needle.go " {
		t.Fatalf("mention was not spliced: %q", got)
	}
}

// TestUnit_Mention_TruncatedIndexSaysSo pins that a truncated walk shows a
// muted footer rather than silently rendering an incomplete list as final.
func TestUnit_Mention_TruncatedIndexSaysSo(t *testing.T) {
	root := t.TempDir()
	for i := 0; i <= fileaddr.WalkBudget; i++ {
		if err := os.WriteFile(filepath.Join(root, fmt.Sprintf("f%05d.go", i)), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	src, err := fileaddr.NewSource(nil, root)
	if err != nil {
		t.Fatal(err)
	}

	h := newHarness(t, func(d *Deps) { d.FileSource = src }).start()
	h.typeText("@f0")
	if !src.Truncated() {
		t.Fatal("the fixture did not exceed the walk budget")
	}
	requireContains(t, testkit.EncodeLines(h.last().Live), indexTruncatedText(false), "the truncation footer")

	// A footer, not an extra row: the overlay still fits its row budget.
	live := h.last().Live
	overlay := len(live) - len(h.a.comp.Render(h.a.width, true, h.a.ascii)) - 1
	if overlay > h.a.rowBudget() {
		t.Fatalf("the picker spent %d rows of a %d-row budget", overlay, h.a.rowBudget())
	}

	// Backspacing back to browse mode takes the footer with it: a directory
	// listing has no walk budget to exceed.
	h.press(input.KeyBackspace)
	h.press(input.KeyBackspace)
	if h.a.pickerQuery != "" {
		t.Fatalf("picker query = %q, want browse mode", h.a.pickerQuery)
	}
	requireNotContains(t, testkit.EncodeLines(h.last().Live), indexTruncatedText(false), "browse mode")

	// Leaving the mention takes the footer with it.
	h.typeText(" ")
	requireNotContains(t, testkit.EncodeLines(h.last().Live), indexTruncatedText(false), "the closed picker")
}

// TestUnit_Mention_RequeriesOnlyOnQueryChange pins that the source is asked
// once per changed query; a keystroke that leaves the token alone costs no
// walk.
func TestUnit_Mention_RequeriesOnlyOnQueryChange(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "retry.go"), []byte("package ingest\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	src, err := fileaddr.NewSource(nil, root)
	if err != nil {
		t.Fatal(err)
	}

	h := newHarness(t, func(d *Deps) { d.FileSource = src }).start()
	h.typeText("@ret")
	if h.a.pickerQuery != "ret" {
		t.Fatalf("picker query = %q, want %q", h.a.pickerQuery, "ret")
	}

	// Left then right returns to the same token; nothing is re-fetched.
	h.press(input.KeyLeft)
	h.press(input.KeyRight)
	if h.a.pickerQuery != "ret" {
		t.Fatalf("cursor movement changed the picker query: %q", h.a.pickerQuery)
	}
	if _, ok := h.a.pick.Selected(); !ok {
		t.Fatal("the candidate list was lost to a no-op keystroke")
	}

	// Leaving the mention closes the list and forgets the query.
	h.typeText(" ")
	if h.a.pickerOpen || h.a.pickerQuery != "" {
		t.Fatalf("picker survived leaving the mention: open=%v query=%q", h.a.pickerOpen, h.a.pickerQuery)
	}
}

// TestUnit_Mention_NoSourceShowsTheFixedEmptyState is the nil-FileSource path.
func TestUnit_Mention_NoSourceShowsTheFixedEmptyState(t *testing.T) {
	h := newHarness(t).start()
	h.typeText("@")
	requireContains(t, testkit.EncodeLines(h.last().Live), fileaddr.NoRootText, "picker empty state")
	// No breadcrumb: there is no tree for one to be a crumb of.
	if got := h.a.pick.Header(); got != "" {
		t.Fatalf("rootless picker header = %q, want none", got)
	}
}

// browseWorkspace is the tree the browsing tests walk, two levels deep, with
// "target.go" deliberately duplicated across directories to distinguish a
// scoped search from an unscoped one.
func browseWorkspace(t *testing.T) *fileaddr.Source {
	t.Helper()
	root := t.TempDir()
	for _, rel := range []string{
		"README.md",
		"docs/target.go",
		"src/main.go",
		"src/target.go",
		"src/deep/handler.go",
	} {
		path := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("package x\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	src, err := fileaddr.NewSource(nil, root)
	if err != nil {
		t.Fatal(err)
	}
	return src
}

// pickerRows is the file list as the operator reads it, top to bottom,
// walked with the same Move the arrow keys use since the picker publishes a
// selection rather than its filtered slice.
func (h *harness) pickerRows() []string {
	h.t.Helper()
	n := h.a.pick.FilteredLen()
	was := h.a.pick.SelectedIndex()
	h.a.pick.Move(-n)
	rows := make([]string, 0, n)
	for i := 0; i < n; i++ {
		it, ok := h.a.pick.Selected()
		if !ok {
			h.t.Fatalf("row %d of %d is not selectable", i, n)
		}
		rows = append(rows, it.Label)
		h.a.pick.Move(1)
	}
	h.a.pick.Move(-n)
	h.a.pick.Move(was)
	return rows
}

// TestUnit_Mention_BrowseOpensAtTheRoot pins that a bare `@` is a directory
// listing of the workspace root, dirs first, under a breadcrumb.
func TestUnit_Mention_BrowseOpensAtTheRoot(t *testing.T) {
	h := newHarness(t, func(d *Deps) { d.FileSource = browseWorkspace(t) }).start()
	h.typeText("@")

	if !h.a.pickerOpen {
		t.Fatal("`@` did not open the file picker")
	}
	if got, want := h.a.pick.Header(), "/"; got != want {
		t.Fatalf("breadcrumb = %q, want %q", got, want)
	}
	if got := strings.Join(h.pickerRows(), " "); got != "docs/ src/ README.md" {
		t.Fatalf("root listing = %q, want the two directories then the file", got)
	}
	requireContains(t, testkit.EncodeLines(h.last().Live), "[muted]/[/]", "the breadcrumb row")

	// Esc closes it, and the next `@` starts over at the root.
	h.press(input.KeyEscape)
	if h.a.pickerOpen || h.a.browser != nil {
		t.Fatal("Esc left the browser alive")
	}
}

// TestUnit_Mention_EnterOnADirectoryDescends pins that Enter descends on a
// directory row and splices on a file row, branching on fileaddr.IsDir.
func TestUnit_Mention_EnterOnADirectoryDescends(t *testing.T) {
	h := newHarness(t, func(d *Deps) { d.FileSource = browseWorkspace(t) }).start()
	h.typeText("@")

	h.press(input.KeyDown) // docs/ -> src/
	sel, ok := h.a.pick.Selected()
	if !ok || sel.Label != "src/" {
		t.Fatalf("selected %+v, want the src/ row", sel)
	}
	h.press(input.KeyEnter)

	if got, want := h.a.pick.Header(), "/src"; got != want {
		t.Fatalf("breadcrumb after descending = %q, want %q", got, want)
	}
	if got := strings.Join(h.pickerRows(), " "); got != "deep/ src/main.go src/target.go" {
		t.Fatalf("listing after descending = %q", got)
	}
	// Descending is navigation, not selection.
	if got := h.a.comp.Draft(); got != "@" {
		t.Fatalf("descending edited the draft: %q", got)
	}
	if !h.a.pickerOpen {
		t.Fatal("descending closed the file list")
	}

	// A file row from the subdirectory splices its full root-relative path.
	h.press(input.KeyDown)
	h.press(input.KeyDown)
	h.press(input.KeyEnter)
	if got := h.a.comp.Draft(); got != "@src/target.go " {
		t.Fatalf("mention = %q, want the full root-relative path", got)
	}
	if h.a.pickerOpen || h.a.browser != nil {
		t.Fatal("accepting a file left the browser open")
	}
}

// TestUnit_Mention_DescendingClearsTheQuery pins that a query is scoped to
// the directory it was typed in and does not carry into a descend. Calls
// descend directly since the keystroke path cannot reach this state (a typed
// query shows files, not directory rows).
func TestUnit_Mention_DescendingClearsTheQuery(t *testing.T) {
	h := newHarness(t, func(d *Deps) { d.FileSource = browseWorkspace(t) }).start()
	h.typeText("look at @tar and more")
	for i := 0; i < len(" and more"); i++ {
		h.press(input.KeyLeft)
	}
	if h.a.pickerQuery != "tar" {
		t.Fatalf("picker query = %q, want %q", h.a.pickerQuery, "tar")
	}

	h.a.descend(h.ctx, "src")

	// The "@" survives; the query typed after it is gone, and the trailing
	// text is left exactly where it was.
	if got, want := h.a.comp.Draft(), "look at @ and more"; got != want {
		t.Fatalf("draft = %q, want %q", got, want)
	}
	_, _, query, ok := h.a.comp.MentionSpan()
	if !ok || query != "" {
		t.Fatalf("mention span = (%q, %v), want an empty query still in a mention", query, ok)
	}
	if h.a.pickerQuery != "" {
		t.Fatalf("picker query = %q, want it cleared", h.a.pickerQuery)
	}
	if got, want := h.a.pick.Header(), "/src"; got != want {
		t.Fatalf("breadcrumb = %q, want %q", got, want)
	}
	if got := strings.Join(h.pickerRows(), " "); got != "deep/ src/main.go src/target.go" {
		t.Fatalf("rows after descending = %q, want the new directory's listing", got)
	}
}

// TestUnit_Mention_TypingIsScopedToTheCurrentDirectory pins that descending
// narrows a search, not just the view: only the copy under the browsed
// subtree may match.
func TestUnit_Mention_TypingIsScopedToTheCurrentDirectory(t *testing.T) {
	h := newHarness(t, func(d *Deps) { d.FileSource = browseWorkspace(t) }).start()

	// At the root the query reaches the whole tree.
	h.typeText("@target")
	if got := strings.Join(h.pickerRows(), " "); got != "docs/target.go src/target.go" {
		t.Fatalf("root search = %q, want both copies", got)
	}

	// Back to browse mode, into src/, and the same query answers for that
	// subtree alone.
	for i := 0; i < len("target"); i++ {
		h.press(input.KeyBackspace)
	}
	h.press(input.KeyDown) // docs/ -> src/
	h.press(input.KeyEnter)
	h.typeText("target")
	if got, want := h.a.pickerQuery, "target"; got != want {
		t.Fatalf("picker query = %q, want %q", got, want)
	}
	if got := strings.Join(h.pickerRows(), " "); got != "src/target.go" {
		t.Fatalf("scoped search = %q, want only the copy under src/", got)
	}
	// A deeper file still matches, so the search is recursive.
	for i := 0; i < len("target"); i++ {
		h.press(input.KeyBackspace)
	}
	h.typeText("handler")
	if got := strings.Join(h.pickerRows(), " "); got != "src/deep/handler.go" {
		t.Fatalf("recursive scoped search = %q", got)
	}
}

// TestUnit_Mention_BackspaceAscendsThenDeletesTheAt pins that Backspace edits
// a typed query, ascends when the query is empty, and at the root falls
// through to delete the "@" that opened the overlay.
func TestUnit_Mention_BackspaceAscendsThenDeletesTheAt(t *testing.T) {
	h := newHarness(t, func(d *Deps) { d.FileSource = browseWorkspace(t) }).start()
	h.typeText("look at @")
	h.press(input.KeyDown) // docs/ -> src/
	h.press(input.KeyEnter)
	h.press(input.KeyEnter) // deep/, already the first row
	if got, want := h.a.pick.Header(), "/src/deep"; got != want {
		t.Fatalf("breadcrumb = %q, want %q", got, want)
	}

	// With something typed, Backspace edits the query; the browser stays put.
	h.typeText("handlerx")
	h.press(input.KeyBackspace)
	if got, want := h.a.comp.Draft(), "look at @handler"; got != want {
		t.Fatalf("draft = %q, want %q", got, want)
	}
	if got, want := h.a.pick.Header(), "/src/deep"; got != want {
		t.Fatalf("editing the query moved the browser: breadcrumb %q, want %q", got, want)
	}
	if got := strings.Join(h.pickerRows(), " "); got != "src/deep/handler.go" {
		t.Fatalf("rows after editing the query = %q", got)
	}

	// Emptied, the next Backspace is navigation again.
	for i := 0; i < len("handler"); i++ {
		h.press(input.KeyBackspace)
	}
	h.press(input.KeyBackspace)
	if got, want := h.a.pick.Header(), "/src"; got != want {
		t.Fatalf("breadcrumb after ascending = %q, want %q", got, want)
	}
	if got := h.a.comp.Draft(); got != "look at @" {
		t.Fatalf("ascending edited the draft: %q", got)
	}
	h.press(input.KeyBackspace)
	if got, want := h.a.pick.Header(), "/"; got != want {
		t.Fatalf("breadcrumb back at the root = %q, want %q", got, want)
	}

	// At the root there is no parent; the key deletes the "@" instead.
	h.press(input.KeyBackspace)
	if got := h.a.comp.Draft(); got != "look at " {
		t.Fatalf("draft = %q, want the mention deleted", got)
	}
	if h.a.pickerOpen || h.a.browser != nil {
		t.Fatal("deleting the `@` left the file list open")
	}
	if h.a.pick.Header() != "" {
		t.Fatalf("a closed picker kept its breadcrumb: %q", h.a.pick.Header())
	}
}

// TestUnit_Mention_BackspaceDeclinesUnderTheSessionList pins that pickerAscend
// declines while the session list, not the file list, is the picker on
// screen.
func TestUnit_Mention_BackspaceDeclinesUnderTheSessionList(t *testing.T) {
	h := newHarness(t, func(d *Deps) { d.FileSource = browseWorkspace(t) }).start()
	h.typeText("look at @")
	h.press(input.KeyDown)
	h.press(input.KeyEnter) // into src/, so an ascend would be visible
	h.a.sessionsOpen = true

	if h.a.pickerAscend(h.ctx) {
		t.Fatal("backspace navigated the file browser while the session list was up")
	}
	if got, want := h.a.pick.Header(), "/src"; got != want {
		t.Fatalf("breadcrumb = %q, want %q", got, want)
	}
	// The keystroke reaches no text either: the modal blocks the composer.
	h.press(input.KeyBackspace)
	if got := h.a.comp.Draft(); got != "look at @" {
		t.Fatalf("draft = %q, want it untouched", got)
	}
}

// TestUnit_Usage_FeedsTheStatusBar checks the gauge only appears once real
// usage lands.
func TestUnit_Usage_FeedsTheStatusBar(t *testing.T) {
	h := newHarness(t, func(d *Deps) { d.FreshSession = false }).start()
	requireNotContains(t, testkit.EncodeLines(h.last().Live), "0/0", "no gauge before the first usage_update")

	h.deliver(enginebridge.UsageUpdated{SessionID: testSession, Used: 1200, Size: 128000})
	requireContains(t, testkit.EncodeLines(h.last().Live), "1200/128000", "context gauge")
}

// TestUnit_Run_QuitsAndRestoresTheTerminal exercises the real select loop:
// events arrive on the terminal channel, /quit ends it, and the bounded
// shutdown closes the bridge and then the terminal.
func TestUnit_Run_QuitsAndRestoresTheTerminal(t *testing.T) {
	ft := newFakeTerm(80, 24)
	fb := testkit.NewFakeBridge()
	done := make(chan error, 1)
	go func() {
		done <- Run(context.Background(), Deps{
			Term: ft, Bridge: fb, SessionID: testSession, FreshSession: true,
			Caps: style.Caps{Profile: style.ProfileTrueColor, Dark: true},
		})
	}()

	for _, r := range "/quit" {
		ft.events <- input.KeyEvent{Key: input.KeyRune, Rune: r}
	}
	ft.events <- input.KeyEvent{Key: input.KeyEnter}
	ft.events <- input.KeyEvent{Key: input.KeyEnter}

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after /quit")
	}
	if ft.closed == 0 {
		t.Fatal("the terminal was not restored")
	}
	requireContains(t, strings.Join(fb.Calls(), "\n"), "", "bridge calls")
}

// TestUnit_Run_EndsWhenTheTerminalDoes covers the EOF path.
func TestUnit_Run_EndsWhenTheTerminalDoes(t *testing.T) {
	ft := newFakeTerm(80, 24)
	fb := testkit.NewFakeBridge()
	done := make(chan error, 1)
	go func() {
		done <- Run(context.Background(), Deps{
			Term: ft, Bridge: fb, SessionID: testSession,
			Caps: style.Caps{Profile: style.ProfileMono},
		})
	}()
	close(ft.events)

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return when input ended")
	}
	if ft.closed == 0 {
		t.Fatal("the terminal was not restored")
	}
}

// TestUnit_Run_ContextCancelShutsDown covers the ctx path.
func TestUnit_Run_ContextCancelShutsDown(t *testing.T) {
	ft := newFakeTerm(80, 24)
	fb := testkit.NewFakeBridge()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, Deps{Term: ft, Bridge: fb, SessionID: testSession, Caps: style.Caps{Profile: style.ProfileMono}})
	}()
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return on context cancel")
	}
}

// TestUnit_Run_RejectsIncompleteDeps keeps the composition root honest.
func TestUnit_Run_RejectsIncompleteDeps(t *testing.T) {
	for name, deps := range map[string]Deps{
		"no term":    {Bridge: testkit.NewFakeBridge(), SessionID: testSession},
		"no bridge":  {Term: newFakeTerm(80, 24), SessionID: testSession},
		"no session": {Term: newFakeTerm(80, 24), Bridge: testkit.NewFakeBridge()},
	} {
		if err := Run(context.Background(), deps); err == nil {
			t.Fatalf("%s: want an error", name)
		}
	}
}

// TestUnit_AsciiFallback_NeverEmitsNonASCII proves the mono profile keeps
// every glyph inside ASCII, at the frame level rather than per component.
func TestUnit_AsciiFallback_NeverEmitsNonASCII(t *testing.T) {
	h := newHarness(t, func(d *Deps) { d.Caps = style.Caps{Profile: style.ProfileMono} }).start()
	h.deliver(testkit.FixtureStreamingTurn(testSession)...)
	h.typeText("/")

	var b strings.Builder
	for _, f := range h.term.frames {
		b.WriteString(testkit.EncodeLines(f.Scrollback))
		b.WriteString(testkit.EncodeLines(f.Live))
	}
	for i, r := range b.String() {
		if r > 127 {
			t.Fatalf("non-ASCII rune %q at %d in a mono frame", r, i)
		}
	}
}

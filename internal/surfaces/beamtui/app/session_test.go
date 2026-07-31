package app

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/contenox/contenox/internal/surfaces/beamtui/enginebridge"
	"github.com/contenox/contenox/internal/surfaces/beamtui/input"
	"github.com/contenox/contenox/internal/surfaces/beamtui/keymap"
	"github.com/contenox/contenox/internal/surfaces/beamtui/testkit"
	libacp "github.com/contenox/libacp"
)

// sessionBridge decorates testkit.FakeBridge with scripted results for
// ListSessions and NewSession; every call still passes through FakeBridge
// first, so the ordered call log stays the shared one.
type sessionBridge struct {
	*testkit.FakeBridge
	roster  []libacp.SessionInfo
	newID   libacp.SessionID
	listErr error
	newErr  error
	loadErr error
}

func (b *sessionBridge) ListSessions(ctx context.Context, req libacp.ListSessionsRequest) (libacp.ListSessionsResponse, error) {
	_, _ = b.FakeBridge.ListSessions(ctx, req)
	if b.listErr != nil {
		return libacp.ListSessionsResponse{}, b.listErr
	}
	return libacp.ListSessionsResponse{Sessions: b.roster}, nil
}

func (b *sessionBridge) NewSession(ctx context.Context, req libacp.NewSessionRequest) (libacp.NewSessionResponse, error) {
	_, _ = b.FakeBridge.NewSession(ctx, req)
	if b.newErr != nil {
		return libacp.NewSessionResponse{}, b.newErr
	}
	return libacp.NewSessionResponse{SessionID: b.newID}, nil
}

func (b *sessionBridge) LoadSession(ctx context.Context, req libacp.LoadSessionRequest) (libacp.LoadSessionResponse, error) {
	_, _ = b.FakeBridge.LoadSession(ctx, req)
	if b.loadErr != nil {
		return libacp.LoadSessionResponse{}, b.loadErr
	}
	return libacp.LoadSessionResponse{}, nil
}

const (
	olderSession  = libacp.SessionID("beam-older")
	secondSession = libacp.SessionID("beam-second")
)

// newSessionHarness is newHarness with the bridge wrapped via a Deps mutator.
func newSessionHarness(t *testing.T, mut ...func(*Deps)) (*harness, *sessionBridge) {
	t.Helper()
	var sb *sessionBridge
	all := []func(*Deps){func(d *Deps) {
		sb = &sessionBridge{
			FakeBridge: d.Bridge.(*testkit.FakeBridge),
			newID:      "beam-fresh",
			roster: []libacp.SessionInfo{
				{SessionID: testSession, Title: "the session under test"},
				{SessionID: olderSession, Title: "rewrite the ingest retry"},
				{SessionID: secondSession},
			},
		}
		d.Bridge = sb
	}}
	all = append(all, mut...)
	h := newHarness(t, all...)
	return h, sb
}

// scrollbackFrom joins the scrollback of every frame committed from index i
// on: what got printed after that point, vs. the harness's whole history.
func scrollbackFrom(h *harness, i int) string {
	var b strings.Builder
	for _, f := range h.term.frames[i:] {
		b.WriteString(testkit.EncodeLines(f.Scrollback))
	}
	return b.String()
}

func (h *harness) runCommand(line string) {
	h.t.Helper()
	h.typeText(line)
	h.press(input.KeyEnter)
}

// TestUnit_NewSession_MintsSwitchesAndReprintsTheWelcome pins /new's bridge
// call order and that the welcome header reprints for the new session.
func TestUnit_NewSession_MintsSwitchesAndReprintsTheWelcome(t *testing.T) {
	h, _ := newSessionHarness(t, func(d *Deps) { d.Cwd = "/work/repo" })
	h.start()
	mark := len(h.term.frames)

	h.runCommand("/new")

	requireOrderedCalls(t, h.calls(),
		"SetActiveSession()",
		`NewSession(cwd="/work/repo")`,
		"SetActiveSession(beam-fresh)",
	)
	if h.a.sessionID != "beam-fresh" {
		t.Fatalf("the loop is still driving %q", h.a.sessionID)
	}
	printed := scrollbackFrom(h, mark)
	requireContains(t, printed, "beam-fresh", "the welcome header names the new session")
	requireNotContains(t, h.calls(), "SubmitPrompt", "/new is client-side")

	// Turns after /new must go to the new id, not the one in Deps.
	h.runCommand("hello there")
	requireContains(t, h.calls(), `SubmitPrompt(beam-fresh, "hello there")`, "prompt routing after /new")
}

// TestUnit_Sessions_PickerShowsTheRoster pins /sessions rendering the roster
// labelled by title, with the active row marked.
func TestUnit_Sessions_PickerShowsTheRoster(t *testing.T) {
	h, _ := newSessionHarness(t)
	h.start()

	h.runCommand("/sessions")
	if !h.a.sessionsOpen {
		t.Fatal("/sessions did not open the switcher")
	}
	requireContains(t, h.calls(), "ListSessions(", "the roster is fetched from the bridge")

	live := testkit.EncodeLines(h.last().Live)
	requireContains(t, live, "rewrite the ingest retry", "a session's title is its label")
	requireContains(t, live, "(active)", "the current session is marked")
	requireContains(t, live, sessionsHint, "the switcher says how to leave")

	requireContains(t, live, string(secondSession), "an untitled session falls back to its id")

	h.press(input.KeyEscape)
	if h.a.sessionsOpen {
		t.Fatal("esc did not close the switcher")
	}
	requireNotContains(t, h.calls(), "LoadSession", "closing the switcher must not switch")
}

// TestUnit_Sessions_EnterSwitchesAndRebuildsTheTranscript pins the bridge
// filter order and that no line from the old session survives the switch.
func TestUnit_Sessions_EnterSwitchesAndRebuildsTheTranscript(t *testing.T) {
	h, _ := newSessionHarness(t)
	h.start()

	// One settled line and one still-streaming tail, both the old session's.
	h.deliver(enginebridge.TextDelta{
		SessionID: testSession, MessageID: "m1",
		Text: "a settled line from the session being left\n",
	})
	h.deliver(enginebridge.TextDelta{
		SessionID: testSession, MessageID: "m1",
		Text: "and a live tail that never settled",
	})
	requireContains(t, testkit.EncodeLines(h.last().Live), "a live tail", "the tail is live before the switch")

	h.runCommand("/sessions")
	h.input(input.KeyEvent{Key: input.KeyRune, Rune: 'j'}) // active row leads; j selects the next row
	mark := len(h.term.frames)
	h.press(input.KeyEnter)

	requireOrderedCalls(t, h.calls(),
		"SetActiveSession()",
		"LoadSession(beam-older)",
		"SetActiveSession(beam-older)",
	)
	if h.a.sessionID != olderSession {
		t.Fatalf("the loop is still driving %q", h.a.sessionID)
	}

	// The rebuilt transcript carries nothing over.
	after := testkit.EncodeLines(h.last().Live)
	requireNotContains(t, after, "a live tail", "the old session's live tail survived the switch")
	requireNotContains(t, scrollbackFrom(h, mark), "a settled line from the session being left",
		"a settled line reprinted into the new session")

	if h.a.messages != 0 || h.a.used != 0 || h.a.size != 0 {
		t.Fatalf("per-session counters leaked: messages=%d used=%d size=%d", h.a.messages, h.a.used, h.a.size)
	}
	if len(h.a.history) != 0 {
		t.Fatalf("the composer's recall list leaked across the switch: %v", h.a.history)
	}

	// The replay the load produces hydrates the new transcript.
	h.deliver(enginebridge.UserEcho{SessionID: olderSession, MessageID: "r1", Text: "replayed from the older session"})
	requireContains(t, h.scrollback(), "replayed from the older session", "the replay hydrates the new transcript")
}

// TestUnit_Sessions_SwitchIsRefusedDuringATurn pins that a switch during a
// running turn is refused with a notice, never silently cancelled.
func TestUnit_Sessions_SwitchIsRefusedDuringATurn(t *testing.T) {
	h, _ := newSessionHarness(t)
	h.start()

	h.runCommand("run something long")
	if !h.a.inFlight {
		t.Fatal("no turn in flight")
	}

	h.chord('s', true, false) // ctrl+s
	h.input(input.KeyEvent{Key: input.KeyRune, Rune: 'j'})
	h.press(input.KeyEnter)

	requireNotContains(t, h.calls(), "LoadSession", "a switch during a turn must not load")
	requireContains(t, h.scrollback(), "a turn is running", "the refusal explains itself")
	if h.a.sessionID != testSession {
		t.Fatalf("the refused switch moved the session to %q", h.a.sessionID)
	}

	// /new is refused on the same terms.
	h.runCommand("/new")
	requireNotContains(t, h.calls(), "NewSession", "/new during a turn must not mint a session")

	// Once the turn ends, the same keystrokes work.
	h.deliver(enginebridge.TurnEnded{SessionID: testSession, StopReason: libacp.StopReasonEndTurn})
	h.chord('s', true, false)
	h.input(input.KeyEvent{Key: input.KeyRune, Rune: 'j'})
	h.press(input.KeyEnter)
	requireContains(t, h.calls(), "LoadSession(beam-older)", "the switch works once the turn is over")
}

// TestUnit_Sessions_ListFailureKeepsYouWhereYouAre pins that a failed roster
// fetch reports the error and does not open the switcher.
func TestUnit_Sessions_ListFailureKeepsYouWhereYouAre(t *testing.T) {
	h, sb := newSessionHarness(t)
	sb.listErr = errors.New("database is gone")
	h.start()

	h.runCommand("/sessions")
	if h.a.sessionsOpen {
		t.Fatal("the switcher opened on a failed roster fetch")
	}
	requireContains(t, h.scrollback(), "database is gone", "the failure is reported")
	requireContains(t, h.scrollback(), "/new", "the notice suggests what still works")
}

// TestUnit_Sessions_LoadFailureRestoresTheFilter pins that a failed load
// restores the active-session filter instead of leaving it unfiltered.
func TestUnit_Sessions_LoadFailureRestoresTheFilter(t *testing.T) {
	h, sb := newSessionHarness(t)
	sb.loadErr = errors.New("unknown session")
	h.start()

	h.runCommand("/sessions")
	h.input(input.KeyEvent{Key: input.KeyRune, Rune: 'j'})
	h.press(input.KeyEnter)

	requireOrderedCalls(t, h.calls(),
		"SetActiveSession()",
		"LoadSession(beam-older)",
		"SetActiveSession(beam-test-session)",
	)
	if h.a.sessionID != testSession {
		t.Fatalf("a failed load moved the session to %q", h.a.sessionID)
	}
	requireContains(t, h.scrollback(), "you are still on", "the failure says where you ended up")
}

// TestUnit_Rename_ForwardsToTheAgentAndRelabels pins that /rename sends the
// line verbatim and the status bar adopts the new title immediately.
func TestUnit_Rename_ForwardsToTheAgentAndRelabels(t *testing.T) {
	h, _ := newSessionHarness(t)
	h.start()
	requireContains(t, testkit.EncodeLines(h.last().Live), "beam-0001", "the status bar starts on the session name")

	h.runCommand("/rename the ingest rewrite")

	requireContains(t, h.calls(), `SubmitPrompt(beam-test-session, "/rename the ingest rewrite")`,
		"rename is persisted server-side so every surface sees it")
	requireContains(t, testkit.EncodeLines(h.last().Live), "the ingest rewrite", "the status bar adopts the new title")
	requireNotContains(t, testkit.EncodeLines(h.last().Live), "beam-0001", "the old name is gone from the bar")

	// `-` hands the label back to the derived title.
	h.deliver(enginebridge.TurnEnded{SessionID: testSession, StopReason: libacp.StopReasonEndTurn})
	h.runCommand("/rename -")
	requireContains(t, testkit.EncodeLines(h.last().Live), "beam-0001", "reset falls back to the session name")
}

// TestUnit_Session_TitleFromTheServerReachesTheStatusBar pins that a title
// published for the current session labels the bar, and one for another
// session never does.
func TestUnit_Session_TitleFromTheServerReachesTheStatusBar(t *testing.T) {
	h, _ := newSessionHarness(t)
	h.start()

	h.a.setSessionTitle(olderSession, "somebody else's session")
	if got := h.a.sessionLabel(); got != "beam-0001" {
		t.Fatalf("a title for another session was adopted: %q", got)
	}

	h.a.setSessionTitle(testSession, "rewrite the ingest retry")
	if got := h.a.sessionLabel(); got != "rewrite the ingest retry" {
		t.Fatalf("the server's title did not reach the label: %q", got)
	}
	h.input(input.ResizeEvent{Width: 80, Height: 24})
	requireContains(t, testkit.EncodeLines(h.last().Live), "rewrite the ingest retry", "status bar")

	// Opening the switcher re-reads the roster, the freshest title source.
	h.a.sessionTitle = ""
	h.runCommand("/sessions")
	if got := h.a.sessionLabel(); got != "the session under test" {
		t.Fatalf("the roster's title was not adopted for the current session: %q", got)
	}
}

// TestUnit_Session_UuidLabelIsShortenedUntilATitleLands pins that a fresh
// session's status-bar label is a shortened uuid until a title lands.
func TestUnit_Session_UuidLabelIsShortenedUntilATitleLands(t *testing.T) {
	const full = "beam-20a88ab8-4f2e-4b0d-9c31-6f1a2b3c4d5e"
	h, _ := newSessionHarness(t, func(d *Deps) { d.SessionName = full })
	h.start()

	if got := h.a.sessionLabel(); got != "beam-20a88ab8" {
		t.Fatalf("session label = %q, want %q", got, "beam-20a88ab8")
	}
	live := testkit.EncodeLines(h.last().Live)
	requireContains(t, live, "beam-20a88ab8", "the status bar")
	requireNotContains(t, live, full, "the full uuid on the status bar")

	// A title replaces the id outright once the server publishes one.
	h.a.setSessionTitle(testSession, "rewrite the ingest retry")
	h.input(input.ResizeEvent{Width: 80, Height: 24})
	requireContains(t, testkit.EncodeLines(h.last().Live), "rewrite the ingest retry", "the status bar")

	// A name that is not id-shaped is left alone.
	for _, name := range []string{"beam-0001", "notes", "the ingest rewrite"} {
		if got := shortSessionName(name); got != name {
			t.Fatalf("shortSessionName(%q) = %q, want it untouched", name, got)
		}
	}
}

// TestUnit_Sessions_BindingIsRegisteredAndDiscoverable pins that ctrl+s and
// the session locals are discoverable via the registry, /help and the bare
// "/" menu, with no separate hand-written text.
func TestUnit_Sessions_BindingIsRegisteredAndDiscoverable(t *testing.T) {
	r := keymap.NewRegistry()
	registerBindings(r) // panics on a collision — ctrl+s must be unclaimed

	var found bool
	for _, e := range r.Help(helpScopes) {
		for _, k := range e.Keys {
			if k == "ctrl+s" {
				found = true
				if e.Help == "" || e.Owner != ownerSessions {
					t.Fatalf("ctrl+s registered without help/owner: %+v", e)
				}
			}
		}
	}
	if !found {
		t.Fatal("ctrl+s is not registered")
	}

	h, _ := newSessionHarness(t)
	h.start()
	h.runCommand("/help")
	printed := h.scrollback()
	for _, want := range []string{"/new", "/sessions", "/rename", "switch sessions"} {
		requireContains(t, printed, want, "/help is a projection of the registry and the palette")
	}

	// The bare `/` menu lists them too.
	h.typeText("/")
	requireContains(t, testkit.EncodeLines(h.last().Live), "sessions", "the bare / menu")
}

// TestUnit_Sessions_CtrlSOpensAndJKMoveOnlyThere pins that j/k steer the
// session switcher while it owns ScopePicker, and are ordinary letters
// everywhere else, including the file picker sharing the same scope.
func TestUnit_Sessions_CtrlSOpensAndJKMoveOnlyThere(t *testing.T) {
	h, _ := newSessionHarness(t)
	h.start()

	h.chord('s', true, false)
	if !h.a.sessionsOpen {
		t.Fatal("ctrl+s did not open the switcher")
	}
	if !h.last().Cursor.Hidden {
		t.Fatal("the caret must hide while the switcher owns the keyboard")
	}

	// The active row leads.
	if it, ok := h.a.sessions.Selected(); !ok || it.ID != string(testSession) {
		t.Fatalf("the switcher did not open on the active session: %+v", it)
	}
	h.input(input.KeyEvent{Key: input.KeyRune, Rune: 'j'})
	if it, _ := h.a.sessions.Selected(); it.ID != string(olderSession) {
		t.Fatalf("j did not move the selection: %+v", it)
	}
	h.input(input.KeyEvent{Key: input.KeyRune, Rune: 'k'})
	if it, _ := h.a.sessions.Selected(); it.ID != string(testSession) {
		t.Fatalf("k did not move the selection back: %+v", it)
	}
	// Typing cannot reach the composer while the switcher is up.
	if !h.a.comp.Empty() {
		t.Fatalf("the switcher leaked keystrokes into the composer: %q", h.a.comp.Draft())
	}
	h.press(input.KeyEscape)

	// With the file picker open the same keys are text.
	h.typeText("look at @j")
	if !h.a.pickerOpen {
		t.Fatal("`@` did not open the file picker")
	}
	h.input(input.KeyEvent{Key: input.KeyRune, Rune: 'k'})
	if got := h.a.comp.Draft(); got != "look at @jk" {
		t.Fatalf("j/k were stolen from a file-picker query: %q", got)
	}
}

// requireOrderedCalls asserts want appears in the bridge's call log in this
// order (other calls may interleave).
func requireOrderedCalls(t *testing.T, log string, want ...string) {
	t.Helper()
	rest := log
	for _, w := range want {
		i := strings.Index(rest, w)
		if i < 0 {
			t.Fatalf("missing %q (in this order: %v) in call log:\n%s", w, want, log)
		}
		rest = rest[i+len(w):]
	}
}

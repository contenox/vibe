package acpsvc

import (
	"context"
	"encoding/json"
	"sync"
	"testing"

	"github.com/contenox/beam/internal/services/shellsession"
	libacp "github.com/contenox/beam/libacp"
	"github.com/stretchr/testify/require"
)

// fakeShellManager is a shellsession.Manager that records the calls the
// transport makes and reproduces the one behaviour under test: every Subscribe
// opens with a Reset chunk carrying the whole scrollback. That is what makes a
// redundant re-subscribe expensive, so counting Resets counts full-scrollback
// repaints.
type fakeShellManager struct {
	mu         sync.Mutex
	subscribes int
	cancels    int
	resets     int
	runs       []string
	resizes    []fakeResize
	live       []func(shellsession.Chunk)
}

type fakeResize struct {
	sessionID string
	rows      int
	cols      int
}

func (f *fakeShellManager) Run(_ context.Context, _, line string) (shellsession.RunResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.runs = append(f.runs, line)
	// Every run adds output, so a later Reset would replay everything before it.
	for _, fn := range f.live {
		fn(shellsession.Chunk{Data: line + "-output\n"})
	}
	return shellsession.RunResult{Offset: int64(len(f.runs)), Snapshot: line + "-output\n"}, nil
}

func (f *fakeShellManager) Read(string, int64, int) shellsession.ReadResult {
	return shellsession.ReadResult{Exists: true}
}

func (f *fakeShellManager) Subscribe(_ string, fn func(shellsession.Chunk)) func() {
	f.mu.Lock()
	f.subscribes++
	f.resets++
	idx := len(f.live)
	f.live = append(f.live, fn)
	f.mu.Unlock()
	fn(shellsession.Chunk{Reset: true, Data: "<entire scrollback>"})
	return func() {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.cancels++
		f.live[idx] = func(shellsession.Chunk) {}
	}
}

func (f *fakeShellManager) Resize(sessionID string, rows, cols int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.resizes = append(f.resizes, fakeResize{sessionID: sessionID, rows: rows, cols: cols})
}

func (f *fakeShellManager) Kill(string) {}
func (f *fakeShellManager) Shutdown()   {}

func (f *fakeShellManager) counts() (subscribes, cancels, resets int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.subscribes, f.cancels, f.resets
}

var _ shellsession.Manager = (*fakeShellManager)(nil)

// terminalTestTransport builds a Transport with just enough state for
// handleTerminalRun and one open session. conn stays nil, so sendUpdate is a
// no-op — the assertions are on what the transport asks the shell manager for,
// which is where the cost lives.
func terminalTestTransport(shells shellsession.Manager) (*Transport, libacp.SessionID, string) {
	sid := libacp.SessionID("sess-acp")
	internalID := "sess-internal"
	t := &Transport{
		deps:     Deps{ShellSessions: shells},
		sessions: map[libacp.SessionID]*sessionEntry{sid: {InternalSessionID: internalID}},
		termSubs: make(map[libacp.SessionID]func()),
	}
	return t, sid, internalID
}

func runTerminal(t *testing.T, tr *Transport, p terminalRunParams) {
	t.Helper()
	raw, err := json.Marshal(p)
	require.NoError(t, err)
	_, rpcErr := tr.handleTerminalRun(context.Background(), raw)
	require.Nil(t, rpcErr)
}

// TestUnit_TerminalRun_ReusesTheLiveSubscription is the regression test for the
// quadratic replay: handleTerminalRun used to call subscribeTerminal, which is
// cancel-and-replace, so EVERY `!` line tore down the stream and opened a new
// one — and every new subscription starts with a Reset carrying the entire
// scrollback. Three commands meant three full repaints of everything typed so
// far, which is what made `!echo AAA` look like it was replaying the session.
//
// The invariant: N runs in one session cost exactly ONE subscription, hence one
// Reset, no matter how many lines are typed.
func TestUnit_TerminalRun_ReusesTheLiveSubscription(t *testing.T) {
	shells := &fakeShellManager{}
	tr, sid, _ := terminalTestTransport(shells)

	for _, cmd := range []string{"echo one", "echo two", "echo three"} {
		runTerminal(t, tr, terminalRunParams{SessionID: string(sid), Command: cmd})
	}

	subs, cancels, resets := shells.counts()
	require.Equal(t, 1, subs, "three `!` lines must share one subscription")
	require.Equal(t, 0, cancels, "reusing a live stream must never cancel it")
	require.Equal(t, 1, resets,
		"exactly one full-scrollback Reset total; one per run is the quadratic replay bug")
}

// TestUnit_TerminalRun_SubscribesWhenNothingIsListening keeps the other half of
// the contract honest: reuse must not turn into "never subscribe". A session
// bound to an external agent never subscribes at session/new, so the first `!`
// line is what starts the panel stream.
func TestUnit_TerminalRun_SubscribesWhenNothingIsListening(t *testing.T) {
	shells := &fakeShellManager{}
	tr, sid, _ := terminalTestTransport(shells)

	require.Empty(t, tr.termSubs, "precondition: no live subscription")
	runTerminal(t, tr, terminalRunParams{SessionID: string(sid), Command: "echo first"})

	subs, _, resets := shells.counts()
	require.Equal(t, 1, subs, "the first run must start the stream")
	require.Equal(t, 1, resets)
	require.Contains(t, tr.termSubs, sid)
}

// TestUnit_TerminalReconnect_StillResets pins where the Reset legitimately
// belongs. A client that reconnects or reloads a session has no buffer, so the
// replace-and-repaint path (session/new, session/load, reconnect — all calling
// subscribeTerminal directly) must keep re-delivering the scrollback and must
// cancel the stale subscription so only one stream is live per session.
func TestUnit_TerminalReconnect_StillResets(t *testing.T) {
	shells := &fakeShellManager{}
	tr, sid, internalID := terminalTestTransport(shells)

	runTerminal(t, tr, terminalRunParams{SessionID: string(sid), Command: "echo before-reload"})
	subs, cancels, resets := shells.counts()
	require.Equal(t, 1, subs)
	require.Equal(t, 0, cancels)
	require.Equal(t, 1, resets)

	// The session/load path.
	tr.subscribeTerminal(sid, internalID)

	subs, cancels, resets = shells.counts()
	require.Equal(t, 2, subs, "a reconnect must open a fresh stream")
	require.Equal(t, 1, cancels, "and cancel the stale one, so exactly one stream stays live")
	require.Equal(t, 2, resets, "a reconnecting client has no buffer, so it must be re-sent the scrollback")

	// And a run after the reload reuses the reconnect's subscription rather than
	// replacing it again.
	runTerminal(t, tr, terminalRunParams{SessionID: string(sid), Command: "echo after-reload"})
	subs, _, resets = shells.counts()
	require.Equal(t, 2, subs)
	require.Equal(t, 2, resets)
}

// TestUnit_TerminalRun_ForwardsClientGeometry covers the additive resize seam:
// the client reports the window it is looking at on the run itself, and the
// size must be applied BEFORE the line is submitted or the command formats
// against the old width.
func TestUnit_TerminalRun_ForwardsClientGeometry(t *testing.T) {
	shells := &fakeShellManager{}
	tr, sid, internalID := terminalTestTransport(shells)

	runTerminal(t, tr, terminalRunParams{
		SessionID: string(sid), Command: "ls", Rows: 42, Cols: 137,
	})

	shells.mu.Lock()
	resizes := append([]fakeResize(nil), shells.resizes...)
	runs := append([]string(nil), shells.runs...)
	shells.mu.Unlock()

	require.Equal(t, []fakeResize{{sessionID: internalID, rows: 42, cols: 137}}, resizes,
		"the geometry must reach the shell manager keyed by the INTERNAL session id")
	require.Equal(t, []string{"ls"}, runs)
}

// TestUnit_TerminalRun_GeometryIsOptional: rows/cols are additive, so a client
// that predates them (or a panel that does not know its size) keeps working
// unchanged. The manager treats a non-positive dimension as "no report".
func TestUnit_TerminalRun_GeometryIsOptional(t *testing.T) {
	shells := &fakeShellManager{}
	tr, sid, _ := terminalTestTransport(shells)

	raw := json.RawMessage(`{"sessionId":"` + string(sid) + `","command":"ls"}`)
	_, rpcErr := tr.handleTerminalRun(context.Background(), raw)
	require.Nil(t, rpcErr, "a run without rows/cols must still succeed")

	shells.mu.Lock()
	defer shells.mu.Unlock()
	require.Equal(t, []string{"ls"}, shells.runs)
	require.Equal(t, []fakeResize{{sessionID: "sess-internal", rows: 0, cols: 0}}, shells.resizes,
		"an unreported size is passed through as 0 and ignored by the manager, not guessed at here")
}

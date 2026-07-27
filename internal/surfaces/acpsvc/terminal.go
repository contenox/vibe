package acpsvc

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/contenox/beam/internal/services/shellsession"
	"github.com/contenox/beam/internal/store/runtimetypes"
	libacp "github.com/contenox/beam/libacp"
)

const (
	// TerminalOutputMetaKey is the `_meta` key under which contenox streams live
	// shell-session output over the ACP WebSocket. It rides a session/update
	// notification whose sessionUpdate discriminator is TerminalOutputUpdateKind;
	// the whole thing lives in the spec's reserved `_meta` namespace, exactly like
	// WorkspaceConfigOptionsMetaKey — a conformant foreign client that does not
	// recognize the key (or the extension update kind) ignores it and keeps
	// working. The payload is a terminalOutputPayload: {sessionId, offset, chunk,
	// reset}. See docs/development/blueprints/beam/shell-sessions.md.
	TerminalOutputMetaKey = "contenox.terminalOutput"

	// TerminalOutputUpdateKind is the extension session/update discriminator that
	// carries a TerminalOutputMetaKey payload. Underscore-prefixed to mark it an
	// extension (mirroring libacp.ExtensionMethodPrefix); unknown to conformant
	// clients, which skip it.
	TerminalOutputUpdateKind libacp.SessionUpdateKind = "_contenox.terminalOutput"

	// extMethodTerminalRun is the `!` passthrough entrypoint: beam runs one user
	// line WITHOUT an LLM turn. User lines are not HITL-gated (the user's own
	// machine and keyboard) but are recorded in the same scrollback the agent
	// reads and the panel streams.
	extMethodTerminalRun = "_contenox/terminal/run"
)

// terminalOutputPayload is the wire shape carried under TerminalOutputMetaKey.
type terminalOutputPayload struct {
	SessionID string `json:"sessionId"`
	Offset    int64  `json:"offset"`
	Chunk     string `json:"chunk"`
	// Reset marks the initial scrollback snapshot delivered on (re)subscribe, so
	// the client replaces its buffer rather than appending — the reconnect story.
	Reset bool `json:"reset,omitempty"`
}

type terminalRunParams struct {
	SessionID string `json:"sessionId"`
	Command   string `json:"command"`
	// Rows/Cols are the client's CURRENT terminal geometry, optional and
	// additive: a client that omits them (or sends 0) gets the previous behavior
	// unchanged. When present they are applied to the session's shell before the
	// line is submitted, so width-aware commands format against the window the
	// user is actually looking at instead of a hardcoded default.
	//
	// This rides on the run request rather than getting its own
	// `_contenox/terminal/resize` method deliberately. Piggy-backing is the
	// smaller change — no new method to route, register, or version — and it
	// covers the case that actually matters: the geometry a command formats
	// against is the geometry at the moment it runs. The cost is that a window
	// resized WHILE something is running gets no SIGWINCH until the next line.
	// Should the panel need that, shellsession.Manager.Resize is already the
	// seam — a `_contenox/terminal/resize` method would be a handler that calls
	// it and nothing else, additive to this.
	Rows int `json:"rows,omitempty"`
	Cols int `json:"cols,omitempty"`
}

type terminalRunResult struct {
	Offset  int64  `json:"offset"`
	Started bool   `json:"started,omitempty"`
	Output  string `json:"output,omitempty"`
}

// handleExtRequest dispatches inbound ACP extension requests. Only the contenox
// terminal namespace is claimed; everything else is MethodNotFound so foreign
// extensions stay unhandled exactly as before.
func (t *Transport) handleExtRequest(ctx context.Context, method string, params json.RawMessage) (json.RawMessage, *libacp.Error) {
	switch method {
	case extMethodTerminalRun:
		return t.handleTerminalRun(ctx, params)
	default:
		return nil, libacp.MethodNotFound(method)
	}
}

func (t *Transport) handleTerminalRun(ctx context.Context, params json.RawMessage) (json.RawMessage, *libacp.Error) {
	if t.deps.ShellSessions == nil {
		return nil, libacp.NewError(libacp.ErrMethodNotFound, "shell sessions are not enabled on this server")
	}
	var p terminalRunParams
	if len(params) > 0 {
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, libacp.NewErrorf(libacp.ErrInvalidParams, "invalid params: %v", err)
		}
	}
	sid := libacp.SessionID(p.SessionID)
	if sid == "" {
		return nil, libacp.NewError(libacp.ErrInvalidParams, "sessionId is required")
	}
	if strings.TrimSpace(p.Command) == "" {
		return nil, libacp.NewError(libacp.ErrInvalidParams, "command is required")
	}
	entry, ok := t.sessionFor(sid)
	if !ok {
		return nil, libacp.NewErrorf(libacp.ErrInvalidParams, "unknown session %q", sid)
	}
	// Make sure live output flows even if the panel opens after this run — but
	// only subscribe when nothing is listening yet. subscribeTerminal is
	// cancel-and-replace, and every fresh subscription opens with a Reset chunk
	// carrying the ENTIRE scrollback; doing that per `!` line made the panel
	// repaint all prior output on every command, so N commands cost O(N^2) bytes
	// on the wire and the transcript read as if each line replayed the session.
	// Reuse keeps the Reset where it belongs: genuine (re)connects, which go
	// through subscribeTerminal directly from session/new and session/load.
	t.ensureTerminalSubscribed(sid, entry.InternalSessionID)

	// Adopt the client's current geometry before the line is typed, so the
	// command formats against the window the user is looking at. A no-op when the
	// client sends nothing, and cheap when the size has not changed.
	t.deps.ShellSessions.Resize(entry.InternalSessionID, p.Rows, p.Cols)

	// Root the shell at the session's workspace via the same cwd resolver the
	// agent tools use: the resolver reads the internal session id from ctx.
	runCtx := context.WithValue(ctx, runtimetypes.SessionIDContextKey, entry.InternalSessionID)
	res, err := t.deps.ShellSessions.Run(runCtx, entry.InternalSessionID, p.Command)
	if err != nil {
		return nil, libacp.InternalError(err.Error())
	}
	out, mErr := json.Marshal(terminalRunResult{Offset: res.Offset, Started: res.Started, Output: res.Snapshot})
	if mErr != nil {
		return nil, libacp.InternalError(mErr.Error())
	}
	return out, nil
}

// subscribeTerminal begins forwarding a session's shell output to the client as
// TerminalOutputMetaKey session/update notifications. Idempotent per ACP session
// id: an existing subscription is cancelled and replaced, so exactly one stream
// is live per session on this connection.
//
// Replacing re-delivers the whole scrollback as a Reset, which is the point on
// the reconnect/reload path (session/new, session/load, session reconnect) and
// is wrong everywhere else — see ensureTerminalSubscribed.
func (t *Transport) subscribeTerminal(sid libacp.SessionID, internalID string) {
	if t.deps.ShellSessions == nil || internalID == "" {
		return
	}
	cancel := t.deps.ShellSessions.Subscribe(internalID, func(c shellsession.Chunk) {
		t.sendTerminalChunk(sid, c)
	})
	t.termSubMu.Lock()
	if prev, ok := t.termSubs[sid]; ok {
		prev()
	}
	t.termSubs[sid] = cancel
	t.termSubMu.Unlock()
}

// ensureTerminalSubscribed subscribes a session's shell output to the client
// only when no subscription is live yet, unlike subscribeTerminal which always
// cancels-and-replaces. Every subscription begins with a full-scrollback Reset,
// so "replace" is only ever the right move for a client that has lost its buffer
// — a reconnect or a session reload, where the native path calls
// subscribeTerminal directly.
//
// Two callers need the cheap form. The external-agent terminal bridge calls it
// on every terminal/create (an external session never subscribes at session/new
// — see NewSession's external branch — so the FIRST create must start the panel
// stream), and the `!` passthrough calls it on every run for the same reason;
// neither may tear down and repaint a panel that is already streaming.
func (t *Transport) ensureTerminalSubscribed(sid libacp.SessionID, internalID string) {
	if t.deps.ShellSessions == nil || internalID == "" {
		return
	}
	t.termSubMu.Lock()
	_, exists := t.termSubs[sid]
	t.termSubMu.Unlock()
	if exists {
		return
	}
	t.subscribeTerminal(sid, internalID)
}

// unsubscribeTerminal stops forwarding a session's shell output.
func (t *Transport) unsubscribeTerminal(sid libacp.SessionID) {
	t.termSubMu.Lock()
	cancel := t.termSubs[sid]
	delete(t.termSubs, sid)
	t.termSubMu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// closeTerminal stops streaming and kills the session's shell. entry may be nil
// when the session was not open on this connection; the shell is keyed by the
// internal session id, so it is killed only when that id is known here (the
// common case — beam closes/deletes the active session). A stray shell for a
// session not open on this connection is left to the idle reaper.
func (t *Transport) closeTerminal(sid libacp.SessionID, entry *sessionEntry) {
	t.unsubscribeTerminal(sid)
	if t.deps.ShellSessions != nil && entry != nil && entry.InternalSessionID != "" {
		t.deps.ShellSessions.Kill(entry.InternalSessionID)
	}
}

func (t *Transport) unsubscribeAllTerminals() {
	t.termSubMu.Lock()
	cancels := make([]func(), 0, len(t.termSubs))
	for _, c := range t.termSubs {
		cancels = append(cancels, c)
	}
	t.termSubs = make(map[libacp.SessionID]func())
	t.termSubMu.Unlock()
	for _, c := range cancels {
		c()
	}
}

func (t *Transport) sendTerminalChunk(sid libacp.SessionID, c shellsession.Chunk) {
	// Suppress empty non-reset chunks (the flusher never emits them, but the
	// initial reset snapshot can be empty for a session with no output yet — that
	// one is still worth sending so the client clears any stale buffer).
	if c.Data == "" && !c.Reset {
		return
	}
	payload := terminalOutputPayload{
		SessionID: string(sid),
		Offset:    c.Offset,
		Chunk:     c.Data,
		Reset:     c.Reset,
	}
	meta := mustJSON(map[string]any{TerminalOutputMetaKey: payload})
	t.sendUpdate(context.Background(), libacp.SessionNotification{
		SessionID: sid,
		Update: libacp.SessionUpdate{
			SessionUpdate: TerminalOutputUpdateKind,
			Meta:          meta,
		},
	})
}

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
	// Make sure live output flows even if the panel opens after this run.
	t.subscribeTerminal(sid, entry.InternalSessionID)

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
// id: an existing subscription is cancelled and replaced (the reconnect/reload
// path), so exactly one stream is live per session on this connection.
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
// cancels-and-replaces. The external-agent terminal bridge calls it on every
// terminal/create (an external session never subscribes at session/new — see
// NewSession's external branch — so the FIRST create must start the panel
// stream), but a second create in the same session must NOT tear down and
// repaint the panel. A replace is only wanted on reconnect/reload, where the
// native path calls subscribeTerminal directly.
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

package acpsvc

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/contenox/contenox/internal/services/shellsession"
	"github.com/contenox/contenox/internal/store/runtimetypes"
	libacp "github.com/contenox/contenox/libacp"
)

const (
	// TerminalOutputMetaKey is the `_meta` key carrying a terminalOutputPayload
	// over the ACP WebSocket; unrecognized clients ignore it.
	TerminalOutputMetaKey = "contenox.terminalOutput"

	// TerminalOutputUpdateKind is the extension session/update discriminator
	// carrying a TerminalOutputMetaKey payload; unknown clients skip it.
	TerminalOutputUpdateKind libacp.SessionUpdateKind = "_contenox.terminalOutput"

	// extMethodTerminalRun is the `!` passthrough: runs one user line without an
	// LLM turn, unguarded by HITL, recorded in the shared scrollback.
	extMethodTerminalRun = "_contenox/terminal/run"
)

// terminalOutputPayload is the wire shape carried under TerminalOutputMetaKey.
type terminalOutputPayload struct {
	SessionID string `json:"sessionId"`
	Offset    int64  `json:"offset"`
	Chunk     string `json:"chunk"`
	// Reset marks a full-scrollback snapshot on (re)subscribe, so the client
	// replaces its buffer rather than appending.
	Reset bool `json:"reset,omitempty"`
}

type terminalRunParams struct {
	SessionID string `json:"sessionId"`
	Command   string `json:"command"`
	// Rows/Cols are the client's current terminal geometry, optional and
	// additive: omitted (or zero) preserves prior behavior. Applied to the
	// session's shell before the line is submitted, so the command formats
	// against the window the user is looking at.
	Rows int `json:"rows,omitempty"`
	Cols int `json:"cols,omitempty"`
}

type terminalRunResult struct {
	Offset  int64  `json:"offset"`
	Started bool   `json:"started,omitempty"`
	Output  string `json:"output,omitempty"`
}

// handleExtRequest dispatches inbound ACP extension requests. Only the
// contenox namespace (terminal + autocomplete + fs listing) is claimed; everything else is
// MethodNotFound.
func (t *Transport) handleExtRequest(ctx context.Context, method string, params json.RawMessage) (json.RawMessage, *libacp.Error) {
	switch method {
	case extMethodTerminalRun:
		return t.handleTerminalRun(ctx, params)
	case extMethodAutocomplete:
		return t.handleAutocomplete(ctx, params)
	case extMethodFSList:
		return t.handleFSList(ctx, params)
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
	// Subscribe only if nothing is listening yet: a fresh subscription always
	// opens with a full-scrollback Reset, so re-subscribing per line would
	// repaint the transcript. Reuse reserves Reset for genuine reconnects.
	t.ensureTerminalSubscribed(sid, entry.InternalSessionID)

	// Apply the client's geometry before the line runs, so output formats
	// against the current window.
	t.deps.ShellSessions.Resize(entry.InternalSessionID, p.Rows, p.Cols)

	// Root the shell at the session's workspace via the same cwd resolver the
	// agent tools use.
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

// subscribeTerminal forwards a session's shell output to the client as
// TerminalOutputMetaKey updates. Idempotent per session: an existing
// subscription is cancelled and replaced, and the replacement re-delivers the
// whole scrollback as a Reset — correct only on reconnect/reload; see
// ensureTerminalSubscribed for the non-replacing form.
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

// ensureTerminalSubscribed subscribes only if no subscription is live yet,
// unlike subscribeTerminal's cancel-and-replace. Used by callers — the
// external-agent terminal bridge and the `!` passthrough — that may be the
// first to start the stream and must not tear down an already-streaming panel.
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

// closeTerminal stops streaming and kills the session's shell. entry may be
// nil if the session wasn't open on this connection, in which case the shell
// is left to the idle reaper.
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
	// Suppress empty non-reset chunks; an empty reset snapshot still goes out so
	// the client clears any stale buffer.
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

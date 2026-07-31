package acpsvc

import (
	"context"
	"fmt"

	libacp "github.com/contenox/libacp"
)

// ErrSessionBusy reports that a session already has a turn in flight, so an
// out-of-band prompt was not started. It is a "not now", not a failure: the
// caller's human-in-the-loop fallback still applies.
var ErrSessionBusy = fmt.Errorf("acpsvc: the session already has a turn in flight")

// PromptContenoxSession runs an out-of-band turn on a live session, addressed
// by its contenox (internal) id, as if the operator had typed it themselves —
// used when a mission unit's supervisor agent answers on the operator's
// behalf. It refuses a session with a turn already in flight (ErrSessionBusy,
// one turn per session is the invariant) and runs through the ordinary Prompt
// path, so the turn streams into the operator's transcript like any other.
func (t *Transport) PromptContenoxSession(ctx context.Context, contenoxSessionID, text string) error {
	sid, ok := t.acpSessionForContenoxID(contenoxSessionID)
	if !ok {
		return ErrSessionNotLive
	}
	if t.hasInflightPrompt(sid) {
		return ErrSessionBusy
	}
	_, err := t.Prompt(ctx, libacp.PromptRequest{
		SessionID: sid,
		Prompt:    []libacp.ContentBlock{libacp.NewTextContent(text)},
	})
	return err
}

// hasInflightPrompt reports whether a turn is currently running for sid.
func (t *Transport) hasInflightPrompt(sid libacp.SessionID) bool {
	t.promptCancelMu.Lock()
	defer t.promptCancelMu.Unlock()
	_, running := t.promptCancels[sid]
	return running
}

// PromptContenoxSession routes an out-of-band turn to whichever live connection
// owns the session — serve's half, mirroring DeliverToContenoxSession.
func (r *SessionRouter) PromptContenoxSession(ctx context.Context, contenoxSessionID, text string) error {
	tr, ok := r.transportFor(contenoxSessionID)
	if !ok {
		return ErrSessionNotLive
	}
	return tr.PromptContenoxSession(ctx, contenoxSessionID, text)
}

package acpsvc

import (
	"context"
	"fmt"

	libacp "github.com/contenox/beam/libacp"
)

// ErrSessionBusy reports that a session already has a turn in flight, so an
// out-of-band prompt was not started. It is a "not now", not a failure: the
// caller's fallback (a human answering the same question) is always still there.
var ErrSessionBusy = fmt.Errorf("acpsvc: the session already has a turn in flight")

// PromptContenoxSession runs an out-of-band TURN on a live session, addressed by
// its contenox (internal) id — the runtime speaking to a session's agent the way
// its operator would.
//
// It exists for exactly one caller today: a mission unit asked its supervisor a
// question, and the envelope permits the supervising AGENT to answer instead of
// waiting for a human. The question is put to that agent as a turn; it answers by
// calling its mission_answer tool, which resolves the unit's ask and unblocks it.
//
// Two properties make this safe to do behind the operator's back:
//
//   - It REFUSES a session with a turn already running (ErrSessionBusy). One turn
//     per session is this transport's invariant, and quietly interleaving an
//     agent-to-agent exchange with something the operator typed would corrupt both.
//   - It runs the ordinary Prompt path, so the operator SEES it: the turn streams
//     into their transcript like any other, tools and all. An answer given on
//     their behalf must not be invisible to them.
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

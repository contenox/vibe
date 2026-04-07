package chatsessionmodes

import (
	"context"
	"time"

	"github.com/contenox/contenox/libtracker"
	"github.com/contenox/contenox/taskengine"
)

// HTTPChat is the method set used by internalchatapi (supports activity decoration).
type HTTPChat interface {
	CreateChatSession(ctx context.Context, identity string) (chatID string, startedAt time.Time, err error)
	ListChatMessages(ctx context.Context, sessionID string) ([]taskengine.Message, error)
	ListChatSessions(ctx context.Context, identity string) ([]ListedChatSession, error)
	SendTurn(ctx context.Context, in TurnInput) (*TurnResult, error)
}

// Compile-time check: *Service is a valid handler implementation.
var _ HTTPChat = (*Service)(nil)

type httpChatActivityDecorator struct {
	svc     HTTPChat
	tracker libtracker.ActivityTracker
}

// WithHTTPChatActivityTracker wraps an [HTTPChat] with activity logging.
func WithHTTPChatActivityTracker(svc HTTPChat, tracker libtracker.ActivityTracker) HTTPChat {
	return &httpChatActivityDecorator{svc: svc, tracker: tracker}
}

var _ HTTPChat = (*httpChatActivityDecorator)(nil)

func (d *httpChatActivityDecorator) CreateChatSession(ctx context.Context, identity string) (string, time.Time, error) {
	reportErr, reportChange, end := d.tracker.Start(ctx, "create", "chat_session")
	defer end()
	id, started, err := d.svc.CreateChatSession(ctx, identity)
	if err != nil {
		reportErr(err)
		return "", time.Time{}, err
	}
	reportChange(id, map[string]string{"id": id})
	return id, started, nil
}

func (d *httpChatActivityDecorator) ListChatMessages(ctx context.Context, sessionID string) ([]taskengine.Message, error) {
	reportErr, _, end := d.tracker.Start(ctx, "read", "chat_messages", "sessionID", sessionID)
	defer end()
	out, err := d.svc.ListChatMessages(ctx, sessionID)
	if err != nil {
		reportErr(err)
		return nil, err
	}
	return out, nil
}

func (d *httpChatActivityDecorator) ListChatSessions(ctx context.Context, identity string) ([]ListedChatSession, error) {
	reportErr, _, end := d.tracker.Start(ctx, "list", "chat_session")
	defer end()
	out, err := d.svc.ListChatSessions(ctx, identity)
	if err != nil {
		reportErr(err)
		return nil, err
	}
	return out, nil
}

func (d *httpChatActivityDecorator) SendTurn(ctx context.Context, in TurnInput) (*TurnResult, error) {
	reportErr, reportChange, end := d.tracker.Start(ctx, "send_turn", "chat_session",
		"sessionID", in.SessionID, "mode", in.Mode, "explicitChainRef", in.ExplicitChainRef)
	defer end()
	out, err := d.svc.SendTurn(ctx, in)
	if err != nil {
		reportErr(err)
		return nil, err
	}
	reportChange(in.SessionID, map[string]any{
		"mode":          in.Mode,
		"inputTokens":   out.InputTokenCount,
		"outputTokens":  out.OutputTokenCount,
		"responseChars": len(out.Response),
	})
	return out, nil
}

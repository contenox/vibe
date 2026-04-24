package chatsessionmodes

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/contenox/contenox/runtime/messagestore"
	"github.com/contenox/contenox/runtime/taskengine"
	"github.com/google/uuid"
)

// ListedChatMessage is one row for HTTP list/history responses.
type ListedChatMessage struct {
	ID      string
	Role    string
	Content string
	SentAt  time.Time
	IsUser  bool
}

// ListedChatSession is one session row for GET /chats (list).
type ListedChatSession struct {
	ID          string
	StartedAt   time.Time
	LastMessage *ListedChatMessage
}

// CreateChatSession allocates a new chat session index for the given identity.
func (s *Service) CreateChatSession(ctx context.Context, identity string) (chatID string, startedAt time.Time, err error) {
	chatID = uuid.NewString()
	startedAt = time.Now().UTC()
	st := messagestore.New(s.db.WithoutTransaction(), s.workspaceID)
	if err := st.CreateMessageIndex(ctx, chatID, identity); err != nil {
		return "", time.Time{}, err
	}
	return chatID, startedAt, nil
}

// ListChatMessages returns all messages for a session (chronological).
func (s *Service) ListChatMessages(ctx context.Context, sessionID string) ([]taskengine.Message, error) {
	return s.chatManager.ListMessages(ctx, s.db.WithoutTransaction(), sessionID)
}

// ListChatSessions returns sessions for an identity with optional last-message preview.
func (s *Service) ListChatSessions(ctx context.Context, identity string) ([]ListedChatSession, error) {
	st := messagestore.New(s.db.WithoutTransaction(), s.workspaceID)
	sessions, err := st.ListAllSessions(ctx, identity)
	if err != nil {
		return nil, err
	}
	out := make([]ListedChatSession, 0, len(sessions))
	for _, session := range sessions {
		item := ListedChatSession{ID: session.ID, StartedAt: time.Now().UTC()}
		last, lerr := st.LastMessage(ctx, session.ID)
		if lerr == nil && last != nil {
			var parsed taskengine.Message
			if jerr := json.Unmarshal(last.Payload, &parsed); jerr == nil {
				item.LastMessage = &ListedChatMessage{
					ID:      parsed.ID,
					Role:    parsed.Role,
					Content: parsed.Content,
					SentAt:  last.AddedAt,
					IsUser:  parsed.Role == "user",
				}
				item.StartedAt = last.AddedAt
			}
		} else if lerr != nil && !errors.Is(lerr, messagestore.ErrNotFound) {
			return nil, lerr
		}
		out = append(out, item)
	}
	return out, nil
}

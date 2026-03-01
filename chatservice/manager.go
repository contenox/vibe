// Package chatservice provides chat session management and message persistence.
package chatservice

import (
	"context"
	"crypto/sha1"
	"encoding/json"
	"fmt"
	"time"

	libdb "github.com/contenox/vibe/libdbexec"
	"github.com/contenox/vibe/messagestore"
	"github.com/contenox/vibe/taskengine"
)

// Manager coordinates chat message management.
type Manager struct{}

// NewManager creates a new Manager.
func NewManager(store messagestore.Store) *Manager {
	return &Manager{}
}

// AddInstruction inserts a system message into an existing chat index.
func (m *Manager) AddInstruction(ctx context.Context, tx libdb.Exec, id string, sendAt time.Time, message string) error {
	msg := taskengine.Message{
		Role:      "system",
		Content:   message,
		Timestamp: sendAt,
	}
	payload, err := json.Marshal(&msg)
	if err != nil {
		return err
	}
	messageID := msg.ID
	if messageID == "" {
		messageID = generateMessageID(id, &msg)
	}
	return messagestore.New(tx).AppendMessages(ctx, &messagestore.Message{
		ID:      messageID,
		IDX:     id,
		Payload: payload,
		AddedAt: sendAt,
	})
}

// AppendMessage appends a message to an in-memory slice (no DB write).
// Call PersistDiff afterwards to persist the result.
func (m *Manager) AppendMessage(_ context.Context, messages []taskengine.Message, sendAt time.Time, message string, role string) ([]taskengine.Message, error) {
	messages = append(messages, taskengine.Message{
		Role:      role,
		Content:   message,
		Timestamp: sendAt,
	})
	return messages, nil
}

// ListMessages retrieves all stored messages for a given subject ID.
func (m *Manager) ListMessages(ctx context.Context, tx libdb.Exec, subjectID string) ([]taskengine.Message, error) {
	conversation, err := messagestore.New(tx).ListMessages(ctx, subjectID)
	if err != nil {
		return nil, err
	}

	var messages []taskengine.Message
	for _, msg := range conversation {
		var parsedMsg taskengine.Message
		if err := json.Unmarshal(msg.Payload, &parsedMsg); err != nil {
			return nil, fmt.Errorf("failed to unmarshal message: %w", err)
		}
		messages = append(messages, parsedMsg)
	}
	return messages, nil
}

// PersistDiff surgically appends only new messages by comparing existing IDs.
func (m *Manager) PersistDiff(ctx context.Context, tx libdb.Exec, subjectID string, hist []taskengine.Message) error {
	if len(hist) == 0 {
		return nil
	}

	conversation, err := messagestore.New(tx).ListMessages(ctx, subjectID)
	if err != nil {
		return err
	}

	existingIDs := make(map[string]bool)
	for _, msg := range conversation {
		existingIDs[msg.ID] = true
	}

	var newMessages []*messagestore.Message
	for _, msg := range hist {
		if msg.ID == "" {
			msg.ID = generateMessageID(subjectID, &msg)
		}
		if existingIDs[msg.ID] {
			continue
		}
		if msg.Timestamp.IsZero() {
			msg.Timestamp = time.Now().UTC()
		}
		payload, err := json.Marshal(msg)
		if err != nil {
			return fmt.Errorf("failed to marshal message: %w", err)
		}
		newMessages = append(newMessages, &messagestore.Message{
			ID:      msg.ID,
			IDX:     subjectID,
			Payload: payload,
			AddedAt: msg.Timestamp,
		})
	}

	if len(newMessages) > 0 {
		return messagestore.New(tx).AppendMessages(ctx, newMessages...)
	}
	return nil
}

// DeleteSession removes all messages and the index for a session.
func (m *Manager) DeleteSession(ctx context.Context, tx libdb.Exec, sessionID string, identity string) error {
	store := messagestore.New(tx)
	// DeleteMessageIndex cascades to messages via ON DELETE CASCADE.
	if err := store.DeleteMessageIndex(ctx, sessionID, identity); err != nil {
		return fmt.Errorf("failed to delete session index: %w", err)
	}
	return nil
}

// RenameSession updates the human-readable name of a session.
func (m *Manager) RenameSession(ctx context.Context, tx libdb.Exec, sessionID string, name string) error {
	return messagestore.New(tx).RenameSession(ctx, sessionID, name)
}

// generateMessageID creates a deterministic ID from the message content.
func generateMessageID(subjectID string, msg *taskengine.Message) string {
	h := sha1.New()
	h.Write([]byte(subjectID))
	h.Write([]byte(msg.Content))
	h.Write([]byte(msg.Role))
	h.Write([]byte(msg.Timestamp.Format(time.RFC3339)))
	return fmt.Sprintf("%x", h.Sum(nil))
}

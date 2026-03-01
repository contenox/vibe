package messagestore

import (
	"context"
	"time"
)

// Message represents a stored message.
type Message struct {
	ID      string    `json:"id"`
	IDX     string    `json:"idx_id"`
	Payload []byte    `json:"payload"`
	AddedAt time.Time `json:"added_at"`
}

// SessionInfo represents a chat session index row.
type SessionInfo struct {
	ID       string
	Identity string
	Name     string // empty if unnamed
}

// Store defines the data access interface for messages.
type Store interface {
	// Index operations
	CreateMessageIndex(ctx context.Context, id string, identity string) error
	CreateNamedMessageIndex(ctx context.Context, id string, identity string, name string) error
	DeleteMessageIndex(ctx context.Context, id string, identity string) error
	ListMessageStreams(ctx context.Context, identity string) ([]string, error)
	ListMessageIndices(ctx context.Context, identity string) ([]string, error)
	ListAllSessions(ctx context.Context, identity string) ([]SessionInfo, error)
	GetSessionByName(ctx context.Context, identity string, name string) (*SessionInfo, error)
	RenameSession(ctx context.Context, id string, name string) error

	// Message operations
	AppendMessages(ctx context.Context, messages ...*Message) error
	DeleteMessages(ctx context.Context, stream string) error
	ListMessages(ctx context.Context, stream string) ([]*Message, error)
	LastMessage(ctx context.Context, stream string) (*Message, error)
	CountMessages(ctx context.Context, stream string) (int, error)
}

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

// Store defines the data access interface for messages.
type Store interface {
	// Index operations
	CreateMessageIndex(ctx context.Context, id string, identity string) error
	DeleteMessageIndex(ctx context.Context, id string, identity string) error
	ListMessageStreams(ctx context.Context, identity string) ([]string, error)
	ListMessageIndices(ctx context.Context, identity string) ([]string, error)

	// Message operations
	AppendMessages(ctx context.Context, messages ...*Message) error
	DeleteMessages(ctx context.Context, stream string) error
	ListMessages(ctx context.Context, stream string) ([]*Message, error)
	LastMessage(ctx context.Context, stream string) (*Message, error)
}

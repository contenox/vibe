package eventstore

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"time"

	"github.com/contenox/vibe/libdbexec"
)

var ErrNotFound = errors.New("not found")

// Event represents a stored event without exposing partition details
type Event struct {
	ID            string          `json:"id" example:"event-uuid"`
	NID           int64           `json:"nid" example:"1"`
	CreatedAt     time.Time       `json:"created_at" example:"2023-01-01T00:00:00Z"`
	EventType     string          `json:"event_type" example:"github.pull_request"`
	EventSource   string          `json:"event_source" example:"github.com"`
	AggregateID   string          `json:"aggregate_id" example:"aggregate-uuid"`
	AggregateType string          `json:"aggregate_type" example:"github.webhook"`
	Version       int             `json:"version" example:"1"`
	Data          json.RawMessage `json:"data" example:"{}"`
	Metadata      json.RawMessage `json:"metadata" example:"{}"`
}

type MappingConfig struct {
	Path          string `json:"path"`
	EventType     string `json:"eventType"`
	EventSource   string `json:"eventSource"`
	AggregateType string `json:"aggregateType"`
	// Extract aggregate ID from payload using JSON path or field name
	AggregateIDField   string `json:"aggregateIDField"`
	AggregateTypeField string `json:"aggregateTypeField"`
	EventTypeField     string `json:"eventTypeField"`
	EventSourceField   string `json:"eventSourceField"`
	EventIDField       string `json:"eventIDField"`
	// Fixed version or field to extract from
	Version int `json:"version"`
	// Metadata fields to extract from headers/payload
	MetadataMapping map[string]string `json:"metadataMapping"`
}

// RawEvent represents an unprocessed incoming event payload
type RawEvent struct {
	ID         string                 `json:"id"`
	NID        int64                  `json:"nid"`
	ReceivedAt time.Time              `json:"received_at"`
	Path       string                 `json:"path"`
	Headers    map[string]string      `json:"headers,omitempty"`
	Payload    map[string]interface{} `json:"payload"`
}

// Store provides methods for storing and retrieving events
type Store interface {
	AppendEvent(ctx context.Context, event *Event) error
	GetEventsByAggregate(ctx context.Context, eventType string, from, to time.Time, aggregateType, aggregateID string, limit int) ([]Event, error)
	GetEventsByType(ctx context.Context, eventType string, from, to time.Time, limit int) ([]Event, error)
	GetEventsBySource(ctx context.Context, eventType string, from, to time.Time, eventSource string, limit int) ([]Event, error)
	GetEventTypesInRange(ctx context.Context, from, to time.Time, limit int) ([]string, error)
	DeleteEventsByTypeInRange(ctx context.Context, eventType string, from, to time.Time) error

	EnsurePartitionExists(ctx context.Context, ts time.Time) error

	// CreateMapping creates a new mapping config. Returns error if ID already exists.
	CreateMapping(ctx context.Context, config *MappingConfig) error
	// GetMapping retrieves a mapping config by its path (unique identifier)
	GetMapping(ctx context.Context, path string) (*MappingConfig, error)
	// UpdateMapping updates an existing mapping config. Returns error if not found.
	UpdateMapping(ctx context.Context, config *MappingConfig) error
	// DeleteMapping deletes a mapping config by path
	DeleteMapping(ctx context.Context, path string) error
	// ListMappings returns all mapping configs
	ListMappings(ctx context.Context) ([]*MappingConfig, error)

	AppendRawEvent(ctx context.Context, event *RawEvent) error
	GetRawEvent(ctx context.Context, from, to time.Time, nid int64) (*RawEvent, error)
	ListRawEvents(ctx context.Context, from, to time.Time, limit int) ([]*RawEvent, error)
	EnsureRawEventPartitionExists(ctx context.Context, ts time.Time) error
}

// internalEvent represents the database structure with partition key
type internalEvent struct {
	*Event
	PartitionKey string `json:"-"`
}

type internalRawEvent struct {
	ID           string    `json:"id"`
	NID          int64     `json:"nid"`
	ReceivedAt   time.Time `json:"received_at"`
	Path         string    `json:"path"`
	Headers      []byte    `json:"headers,omitempty"` //binary encoded
	Payload      []byte    `json:"payload"`           //binary encoded
	PartitionKey string    `json:"-"`
}

// store implements EventStore using libdbexec
type store struct {
	Exec     libdbexec.Exec
	pManager *partitionManager
}

type partitionManager struct {
	lastExecuted *time.Time
	lock         sync.Mutex
}

// New creates a new event store instance
func New(exec libdbexec.Exec) Store {
	return &store{Exec: exec, pManager: &partitionManager{lock: sync.Mutex{}, lastExecuted: nil}}
}

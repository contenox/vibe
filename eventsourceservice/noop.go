package eventsourceservice

import (
	"context"
	"time"

	"github.com/contenox/vibe/eventstore"
)

// NoopService is a no-op implementation of Service for contexts that do not
// persist events (e.g. vibe CLI with SQLite, where partition-based storage is not available).
// All writes are dropped; reads return empty results or ErrNotFound where appropriate.
type NoopService struct{}

// noopSubscription implements Subscription for SubscribeToEvents.
type noopSubscription struct{}

func (noopSubscription) Unsubscribe() error { return nil }

// NewNoopService returns a Service that does nothing. Useful when event persistence
// is not required (e.g. local CLI) or when the backing store cannot be used (e.g. SQLite without partitions).
func NewNoopService() Service {
	return &NoopService{}
}

func (s *NoopService) AppendEvent(ctx context.Context, event *eventstore.Event) error {
	return nil
}

func (s *NoopService) GetEventsByAggregate(ctx context.Context, eventType string, from, to time.Time, aggregateType, aggregateID string, limit int) ([]eventstore.Event, error) {
	return []eventstore.Event{}, nil
}

func (s *NoopService) GetEventsByType(ctx context.Context, eventType string, from, to time.Time, limit int) ([]eventstore.Event, error) {
	return []eventstore.Event{}, nil
}

func (s *NoopService) GetEventsBySource(ctx context.Context, eventType string, from, to time.Time, eventSource string, limit int) ([]eventstore.Event, error) {
	return []eventstore.Event{}, nil
}

func (s *NoopService) SubscribeToEvents(ctx context.Context, eventType string, ch chan<- []byte) (Subscription, error) {
	return noopSubscription{}, nil
}

func (s *NoopService) AppendRawEvent(ctx context.Context, event *eventstore.RawEvent) error {
	return nil
}

func (s *NoopService) GetRawEvent(ctx context.Context, from, to time.Time, nid int64) (*eventstore.RawEvent, error) {
	return nil, eventstore.ErrNotFound
}

func (s *NoopService) ListRawEvents(ctx context.Context, from, to time.Time, limit int) ([]*eventstore.RawEvent, error) {
	return []*eventstore.RawEvent{}, nil
}

func (s *NoopService) GetEventTypesInRange(ctx context.Context, from, to time.Time, limit int) ([]string, error) {
	return []string{}, nil
}

func (s *NoopService) DeleteEventsByTypeInRange(ctx context.Context, eventType string, from, to time.Time) error {
	return nil
}

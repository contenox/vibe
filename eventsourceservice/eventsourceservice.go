package eventsourceservice

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/contenox/vibe/eventstore"
	"github.com/contenox/vibe/internal/eventdispatch"
	"github.com/contenox/vibe/libbus"
	"github.com/contenox/vibe/libdbexec"
)

// Service defines the interface for the event source service
type Service interface {
	AppendEvent(ctx context.Context, event *eventstore.Event) error
	GetEventsByAggregate(ctx context.Context, eventType string, from, to time.Time, aggregateType, aggregateID string, limit int) ([]eventstore.Event, error)
	GetEventsByType(ctx context.Context, eventType string, from, to time.Time, limit int) ([]eventstore.Event, error)
	GetEventsBySource(ctx context.Context, eventType string, from, to time.Time, eventSource string, limit int) ([]eventstore.Event, error)
	SubscribeToEvents(ctx context.Context, eventType string, ch chan<- []byte) (Subscription, error)
	AppendRawEvent(ctx context.Context, event *eventstore.RawEvent) error
	GetRawEvent(ctx context.Context, from, to time.Time, nid int64) (*eventstore.RawEvent, error)

	ListRawEvents(ctx context.Context, from, to time.Time, limit int) ([]*eventstore.RawEvent, error)

	GetEventTypesInRange(ctx context.Context, from, to time.Time, limit int) ([]string, error)
	DeleteEventsByTypeInRange(ctx context.Context, eventType string, from, to time.Time) error
}

var ErrEventTooNew = errors.New("event is too new")
var ErrEventTooOld = errors.New("event is too old")
var ErrInvalidParameter = errors.New("invalid parameter")
var ErrMissingRequiredField = errors.New("missing required field")

// EventSource implements the event source service with partition management
type EventSource struct {
	dbInstance libdbexec.DBManager
	manager    partitionManager
	rawManager partitionManager
	messenger  libbus.Messenger
	action     eventdispatch.Trigger
}

type partitionManager struct {
	lock                  sync.Mutex
	lastExistingPartition time.Time
	initialized           bool
}

// NewEventSourceService creates a new event source service with partition management
func NewEventSourceService(
	ctx context.Context,
	dbInstance libdbexec.DBManager,
	messenger libbus.Messenger,
	action eventdispatch.Trigger,
) (Service, error) {

	service := &EventSource{
		dbInstance: dbInstance,
		manager: partitionManager{
			initialized: false,
			lock:        sync.Mutex{},
		},
		rawManager: partitionManager{
			initialized: false,
			lock:        sync.Mutex{},
		},
		messenger: messenger,
		action:    action,
	}

	// Initialize partitions for domain events
	if err := service.ensurePartitions(ctx); err != nil {
		return nil, fmt.Errorf("failed to initialize domain event partitions: %w", err)
	}

	// Initialize partitions for raw events
	if err := service.ensureRawPartitions(ctx); err != nil {
		return nil, fmt.Errorf("failed to initialize raw event partitions: %w", err)
	}

	return service, nil
}

func (s *EventSource) ensureRawPartitions(ctx context.Context) error {
	s.rawManager.lock.Lock()
	defer s.rawManager.lock.Unlock()

	if !s.rawManager.initialized {
		now := time.Now().UTC()
		dates := make([]time.Time, 14) // current + next 13 days
		for i := 0; i < 14; i++ {
			dates[i] = now.AddDate(0, 0, i)
		}

		exec := s.dbInstance.WithoutTransaction()
		store := eventstore.New(exec)

		for _, date := range dates {
			if err := store.EnsureRawEventPartitionExists(ctx, date); err != nil {
				return fmt.Errorf("failed to create raw event partition for %v: %w", date, err)
			}
		}

		s.rawManager.lastExistingPartition = now.AddDate(0, 0, 13)
		s.rawManager.initialized = true
	}

	return nil
}

func (s *EventSource) ensureRawEventPartitionsForEvent(ctx context.Context, eventTime time.Time) error {
	s.rawManager.lock.Lock()
	defer s.rawManager.lock.Unlock()

	if !eventTime.After(s.rawManager.lastExistingPartition) {
		return nil // Partition coverage already sufficient
	}

	dates := make([]time.Time, 7)
	for i := 0; i < 7; i++ {
		dates[i] = eventTime.AddDate(0, 0, i)
	}

	exec := s.dbInstance.WithoutTransaction()
	store := eventstore.New(exec)

	for _, date := range dates {
		if err := store.EnsureRawEventPartitionExists(ctx, date); err != nil {
			return fmt.Errorf("failed to create raw event partition for %v: %w", date, err)
		}
	}

	s.rawManager.lastExistingPartition = eventTime.AddDate(0, 0, 6)
	return nil
}

func (s *EventSource) AppendRawEvent(ctx context.Context, event *eventstore.RawEvent) error {
	if event == nil {
		return fmt.Errorf("%w: raw event cannot be nil", ErrInvalidParameter)
	}

	if event.ReceivedAt.IsZero() {
		event.ReceivedAt = time.Now().UTC()
	}

	if event.Path == "" {
		return fmt.Errorf("%w: path is required", ErrMissingRequiredField)
	}

	// Use dedicated raw partition manager
	if err := s.ensureRawEventPartitionsForEvent(ctx, event.ReceivedAt); err != nil {
		return fmt.Errorf("failed to ensure raw event partitions: %w", err)
	}

	exec := s.dbInstance.WithoutTransaction()
	store := eventstore.New(exec)

	if err := store.AppendRawEvent(ctx, event); err != nil {
		return fmt.Errorf("failed to append raw event: %w", err)
	}

	// Optional: async processing or publishing can go here

	return nil
}

func (s *EventSource) GetRawEvent(ctx context.Context, from, to time.Time, nid int64) (*eventstore.RawEvent, error) {
	tx := s.dbInstance.WithoutTransaction()
	store := eventstore.New(tx)
	ev, err := store.GetRawEvent(ctx, from, to, nid)
	if err != nil {
		return nil, err
	}
	return ev, nil
}

type Subscription interface {
	Unsubscribe() error
}

func (s *EventSource) SubscribeToEvents(ctx context.Context, eventType string, ch chan<- []byte) (Subscription, error) {
	subject := fmt.Sprintf("events.%s", eventType)
	return s.messenger.Stream(ctx, subject, ch)
}

func (s *EventSource) ensurePartitions(ctx context.Context) error {
	s.manager.lock.Lock()
	defer s.manager.lock.Unlock()

	if !s.manager.initialized {
		now := time.Now().UTC()
		// Create partitions for current week + next week
		dates := make([]time.Time, 14)
		for i := 0; i < 14; i++ {
			dates[i] = now.AddDate(0, 0, i)
		}
		exec := s.dbInstance.WithoutTransaction()
		store := eventstore.New(exec)

		for _, date := range dates {
			if err := store.EnsurePartitionExists(ctx, date); err != nil {
				return fmt.Errorf("failed to create partition for %v: %w", date, err)
			}
		}

		s.manager.lastExistingPartition = now.AddDate(0, 0, 13) // Two weeks from now
		s.manager.initialized = true
	}

	return nil
}

func (s *EventSource) ensurePartitionsForEvent(ctx context.Context, eventTime time.Time) error {
	s.manager.lock.Lock()
	defer s.manager.lock.Unlock()

	// Double-check pattern to avoid race conditions
	if !eventTime.After(s.manager.lastExistingPartition) {
		return nil // Partition already exists
	}

	// Create partitions for the next week from the event time
	dates := make([]time.Time, 7)
	for i := 0; i < 7; i++ {
		dates[i] = eventTime.AddDate(0, 0, i)
	}
	exec := s.dbInstance.WithoutTransaction()
	store := eventstore.New(exec)

	for _, date := range dates {
		if err := store.EnsurePartitionExists(ctx, date); err != nil {
			return fmt.Errorf("failed to create partition for %v: %w", date, err)
		}
	}

	s.manager.lastExistingPartition = eventTime.AddDate(0, 0, 6)
	return nil
}

// validateEvent validates the event structure before appending
func (s *EventSource) validateEvent(event *eventstore.Event) error {
	if event == nil {
		return fmt.Errorf("%w: event cannot be nil", ErrInvalidParameter)
	}

	if event.EventType == "" {
		return fmt.Errorf("%w: event_type is required", ErrMissingRequiredField)
	}

	if event.AggregateID == "" {
		return fmt.Errorf("%w: aggregate_id is required", ErrMissingRequiredField)
	}

	if event.AggregateType == "" {
		return fmt.Errorf("%w: aggregate_type is required", ErrMissingRequiredField)
	}

	if event.Version <= 0 {
		return fmt.Errorf("%w: version must be greater than 0", ErrInvalidParameter)
	}

	now := time.Now().UTC()

	// Validate CreatedAt is within acceptable range
	if event.CreatedAt.Before(now.Add(-time.Minute * 10)) {
		return ErrEventTooOld
	}
	if event.CreatedAt.After(now.Add(time.Minute * 10)) {
		return ErrEventTooNew
	}

	return nil
}

// validateTimeRange validates that from <= to
func (s *EventSource) validateTimeRange(from, to time.Time) error {
	if from.After(to) {
		return fmt.Errorf("%w: 'from' time (%v) cannot be after 'to' time (%v)", ErrInvalidParameter, from, to)
	}
	return nil
}

// validateLimit validates that limit is positive
func (s *EventSource) validateLimit(limit int) error {
	if limit <= 0 {
		return fmt.Errorf("%w: limit must be positive, got %d", ErrInvalidParameter, limit)
	}
	if limit > 1000 { // Reasonable upper bound
		return fmt.Errorf("%w: limit cannot exceed 1000, got %d", ErrInvalidParameter, limit)
	}
	return nil
}

// AppendEvent implements Service interface
func (s *EventSource) AppendEvent(ctx context.Context, event *eventstore.Event) error {
	// If CreatedAt is not set, set it to now
	if event.CreatedAt.IsZero() {
		event.CreatedAt = time.Now().UTC()
	}

	// Validate event structure
	if err := s.validateEvent(event); err != nil {
		return err
	}

	// Ensure partitions exist for this event
	if err := s.ensurePartitionsForEvent(ctx, event.CreatedAt); err != nil {
		return fmt.Errorf("failed to ensure partitions: %w", err)
	}
	eventData, err := json.Marshal(event)
	if err != nil {
		return fmt.Errorf("failed to marshal event: %w", err)
	}
	exec := s.dbInstance.WithoutTransaction()
	store := eventstore.New(exec)

	err = store.AppendEvent(ctx, event)
	if err != nil {
		return err
	}
	subject := fmt.Sprintf("events.%s", event.EventType)
	go func() {
		bgCtx := context.Background()
		if err := s.messenger.Publish(bgCtx, subject, eventData); err != nil {
			slog.Error("Failed to publish event to NATS", "event_id", event.ID, "error", err)
		}
	}()

	s.action.HandleEvent(ctx, event)

	return nil
}

// GetEventsByAggregate implements Service interface
func (s *EventSource) GetEventsByAggregate(ctx context.Context, eventType string, from, to time.Time, aggregateType, aggregateID string, limit int) ([]eventstore.Event, error) {
	// Validate parameters
	if eventType == "" {
		return nil, fmt.Errorf("%w: event_type is required", ErrMissingRequiredField)
	}
	if aggregateType == "" {
		return nil, fmt.Errorf("%w: aggregate_type is required", ErrMissingRequiredField)
	}
	if aggregateID == "" {
		return nil, fmt.Errorf("%w: aggregate_id is required", ErrMissingRequiredField)
	}
	if err := s.validateTimeRange(from, to); err != nil {
		return nil, err
	}
	if err := s.validateLimit(limit); err != nil {
		return nil, err
	}
	exec := s.dbInstance.WithoutTransaction()
	store := eventstore.New(exec)

	return store.GetEventsByAggregate(ctx, eventType, from, to, aggregateType, aggregateID, limit)
}

// GetEventsByType implements Service interface
func (s *EventSource) GetEventsByType(ctx context.Context, eventType string, from, to time.Time, limit int) ([]eventstore.Event, error) {
	// Validate parameters
	if eventType == "" {
		return nil, fmt.Errorf("%w: event_type is required", ErrMissingRequiredField)
	}
	if err := s.validateTimeRange(from, to); err != nil {
		return nil, err
	}
	if err := s.validateLimit(limit); err != nil {
		return nil, err
	}

	exec := s.dbInstance.WithoutTransaction()
	store := eventstore.New(exec)
	events, err := store.GetEventsByType(ctx, eventType, from, to, limit)
	if err != nil {
		return nil, fmt.Errorf("failed to get events by type: %w", err)
	}
	return events, nil
}

// GetEventsBySource implements Service interface
func (s *EventSource) GetEventsBySource(ctx context.Context, eventType string, from, to time.Time, eventSource string, limit int) ([]eventstore.Event, error) {
	// Validate parameters
	if eventType == "" {
		return nil, fmt.Errorf("%w: event_type is required", ErrMissingRequiredField)
	}
	if eventSource == "" {
		return nil, fmt.Errorf("%w: event_source is required", ErrMissingRequiredField)
	}
	if err := s.validateTimeRange(from, to); err != nil {
		return nil, err
	}
	if err := s.validateLimit(limit); err != nil {
		return nil, err
	}
	exec := s.dbInstance.WithoutTransaction()
	store := eventstore.New(exec)

	return store.GetEventsBySource(ctx, eventType, from, to, eventSource, limit)
}

// GetEventTypesInRange implements Service interface
func (s *EventSource) GetEventTypesInRange(ctx context.Context, from, to time.Time, limit int) ([]string, error) {
	// Validate parameters
	if err := s.validateTimeRange(from, to); err != nil {
		return nil, err
	}
	if err := s.validateLimit(limit); err != nil {
		return nil, err
	}
	exec := s.dbInstance.WithoutTransaction()
	store := eventstore.New(exec)

	return store.GetEventTypesInRange(ctx, from, to, limit)
}

// DeleteEventsByTypeInRange implements Service interface
func (s *EventSource) DeleteEventsByTypeInRange(ctx context.Context, eventType string, from, to time.Time) error {
	// Validate parameters
	if eventType == "" {
		return fmt.Errorf("%w: event_type is required", ErrMissingRequiredField)
	}
	if err := s.validateTimeRange(from, to); err != nil {
		return err
	}

	exec := s.dbInstance.WithoutTransaction()
	store := eventstore.New(exec)

	return store.DeleteEventsByTypeInRange(ctx, eventType, from, to)
}

// ListRawEvents implements Service interface
func (s *EventSource) ListRawEvents(ctx context.Context, from, to time.Time, limit int) ([]*eventstore.RawEvent, error) {
	// Validate parameters
	if err := s.validateTimeRange(from, to); err != nil {
		return nil, err
	}
	if err := s.validateLimit(limit); err != nil {
		return nil, err
	}

	exec := s.dbInstance.WithoutTransaction()
	store := eventstore.New(exec)

	return store.ListRawEvents(ctx, from, to, limit)
}

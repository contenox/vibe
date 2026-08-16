// Package eventlog is the service tier over the durable, append-only domain
// event log, which itself lives in internal/store/runtimetypes. The store is the
// source of truth; the bus is a best-effort live fan-out, and consumers that
// miss a publish catch up from their NID cursor.
package eventlog

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/contenox/contenox/internal/store/runtimetypes"
	libbus "github.com/contenox/contenox/libbus"
	libdb "github.com/contenox/contenox/libdbexec"
	"github.com/contenox/contenox/libtracker"
)

// Service is the event-source surface: durable append with best-effort live
// fan-out, cursor reads, time-range queries, and the live subscription. The bus
// subject is the event's Type verbatim.
type Service interface {
	// Append validates and durably stores event, then publishes the stored
	// envelope on subject event.Type. A failed publish is reported, never returned.
	Append(ctx context.Context, event *runtimetypes.Event) error
	ListEventsSince(ctx context.Context, workspaceID string, afterNID int64, limit int) ([]runtimetypes.Event, error)
	ListRecentEvents(ctx context.Context, workspaceID string, beforeNID int64, limit int) ([]runtimetypes.Event, error)
	GetEventsByType(ctx context.Context, workspaceID, eventType string, from, to time.Time, limit int) ([]runtimetypes.Event, error)
	GetEventsBySource(ctx context.Context, workspaceID, eventType string, from, to time.Time, eventSource string, limit int) ([]runtimetypes.Event, error)
	GetEventsBySubject(ctx context.Context, workspaceID, eventType string, from, to time.Time, subject string, limit int) ([]runtimetypes.Event, error)
	GetEventTypesInRange(ctx context.Context, workspaceID string, from, to time.Time, limit int) ([]string, error)
	DeleteEventsByTypeInRange(ctx context.Context, workspaceID, eventType string, from, to time.Time) error
	// PrunePartitionsBefore drops every partition older than t's period and
	// returns the dropped periods, forgetting them so a later append re-creates one.
	PrunePartitionsBefore(ctx context.Context, t time.Time) ([]string, error)
	// Subscribe attaches ch to the live stream for eventType. Payload shapes vary
	// by producer and the subject carries no workspace, so a consumer needing
	// ordering, dedup or scoping must treat a delivery as a nudge and read the log.
	Subscribe(ctx context.Context, eventType string, ch chan<- []byte) (libbus.Subscription, error)
}

type service struct {
	store      runtimetypes.EventStore
	bus        libbus.Messenger
	tracker    libtracker.ActivityTracker
	trigger    Trigger
	partitions partitionCache
}

type partitionCache struct {
	mu      sync.Mutex
	ensured map[string]bool
}

func (c *partitionCache) has(period string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ensured[period]
}

func (c *partitionCache) mark(period string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ensured[period] = true
}

func (c *partitionCache) forget(periods []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, period := range periods {
		delete(c.ensured, period)
	}
}

// ServiceOption configures NewService.
type ServiceOption func(*service)

// WithTrigger installs the optional in-process trigger hook, which Append calls
// asynchronously after the durable write. A firing error never aborts the append.
func WithTrigger(t Trigger) ServiceOption {
	return func(s *service) {
		if t != nil {
			s.trigger = t
		}
	}
}

// NewService builds the event service. bus may be nil (append-only, no live
// fan-out and no Subscribe); a nil tracker degrades to Noop.
func NewService(db libdb.DBManager, bus libbus.Messenger, tracker libtracker.ActivityTracker, opts ...ServiceOption) Service {
	if tracker == nil {
		tracker = libtracker.NoopTracker{}
	}
	s := &service{
		store:      runtimetypes.NewEventStore(db),
		bus:        bus,
		tracker:    tracker,
		partitions: partitionCache{ensured: map[string]bool{}},
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

func (s *service) Append(ctx context.Context, event *runtimetypes.Event) error {
	if event == nil {
		return fmt.Errorf("%w: event cannot be nil", runtimetypes.ErrInvalidEventParameter)
	}
	if err := s.ensurePartition(ctx, event.Time); err != nil {
		return err
	}
	if err := s.store.AppendEvent(ctx, event); err != nil {
		return err
	}
	// Fires after the durable write, never on the append's error path.
	defer fireTrigger(ctx, s.trigger, event)
	if s.bus == nil {
		return nil
	}
	// Publish after the durable write, best-effort and synchronous.
	payload, err := json.Marshal(event)
	if err == nil {
		err = s.bus.Publish(ctx, event.Type, payload)
	}
	if err != nil {
		reportErr, _, end := s.tracker.Start(ctx, "publish", "event", "type", event.Type, "nid", event.NID)
		reportErr(fmt.Errorf("eventlog: publish after append failed; event %d is durable, live fan-out skipped: %w", event.NID, err))
		end()
	}
	return nil
}

func (s *service) ensurePartition(ctx context.Context, t time.Time) error {
	now := time.Now().UTC()
	if t.IsZero() {
		t = now
	}
	if t.Before(now.Add(-runtimetypes.EventAcceptanceWindow)) || t.After(now.Add(runtimetypes.EventAcceptanceWindow)) {
		return nil
	}
	period := runtimetypes.EventPeriodKey(t)
	if s.partitions.has(period) {
		return nil
	}
	if err := s.store.EnsureEventPartitionExists(ctx, t); err != nil {
		return err
	}
	s.partitions.mark(period)
	return nil
}

func (s *service) PrunePartitionsBefore(ctx context.Context, t time.Time) ([]string, error) {
	dropped, err := s.store.PruneEventPartitionsBefore(ctx, t)
	// Invalidate on the error path too: dropped names the periods already gone.
	s.partitions.forget(dropped)
	return dropped, err
}

func (s *service) ListEventsSince(ctx context.Context, workspaceID string, afterNID int64, limit int) ([]runtimetypes.Event, error) {
	return s.store.ListEventsSince(ctx, workspaceID, afterNID, limit)
}

func (s *service) ListRecentEvents(ctx context.Context, workspaceID string, beforeNID int64, limit int) ([]runtimetypes.Event, error) {
	return s.store.ListRecentEvents(ctx, workspaceID, beforeNID, limit)
}

func (s *service) GetEventsByType(ctx context.Context, workspaceID, eventType string, from, to time.Time, limit int) ([]runtimetypes.Event, error) {
	if err := validateQuery(workspaceID, from, to, limit); err != nil {
		return nil, err
	}
	return s.store.GetEventsByType(ctx, workspaceID, eventType, from, to, limit)
}

func (s *service) GetEventsBySource(ctx context.Context, workspaceID, eventType string, from, to time.Time, eventSource string, limit int) ([]runtimetypes.Event, error) {
	if err := validateQuery(workspaceID, from, to, limit); err != nil {
		return nil, err
	}
	return s.store.GetEventsBySource(ctx, workspaceID, eventType, from, to, eventSource, limit)
}

func (s *service) GetEventsBySubject(ctx context.Context, workspaceID, eventType string, from, to time.Time, subject string, limit int) ([]runtimetypes.Event, error) {
	if err := validateQuery(workspaceID, from, to, limit); err != nil {
		return nil, err
	}
	return s.store.GetEventsBySubject(ctx, workspaceID, eventType, from, to, subject, limit)
}

func (s *service) GetEventTypesInRange(ctx context.Context, workspaceID string, from, to time.Time, limit int) ([]string, error) {
	if err := validateQuery(workspaceID, from, to, limit); err != nil {
		return nil, err
	}
	return s.store.GetEventTypesInRange(ctx, workspaceID, from, to, limit)
}

func (s *service) DeleteEventsByTypeInRange(ctx context.Context, workspaceID, eventType string, from, to time.Time) error {
	if err := requireWorkspace(workspaceID); err != nil {
		return err
	}
	if from.After(to) {
		return fmt.Errorf("%w: from after to", runtimetypes.ErrInvalidEventParameter)
	}
	return s.store.DeleteEventsByTypeInRange(ctx, workspaceID, eventType, from, to)
}

func (s *service) Subscribe(ctx context.Context, eventType string, ch chan<- []byte) (libbus.Subscription, error) {
	if s.bus == nil {
		return nil, fmt.Errorf("eventlog: no bus wired for subscriptions")
	}
	return s.bus.Stream(ctx, eventType, ch)
}

func validateQuery(workspaceID string, from, to time.Time, limit int) error {
	if err := requireWorkspace(workspaceID); err != nil {
		return err
	}
	if from.After(to) {
		return fmt.Errorf("%w: 'from' (%v) after 'to' (%v)", runtimetypes.ErrInvalidEventParameter, from, to)
	}
	if limit <= 0 {
		return fmt.Errorf("%w: limit must be positive", runtimetypes.ErrInvalidEventParameter)
	}
	if limit > runtimetypes.MaxEventListLimit {
		return fmt.Errorf("%w: limit cannot exceed %d", runtimetypes.ErrInvalidEventParameter, runtimetypes.MaxEventListLimit)
	}
	return nil
}

func requireWorkspace(workspaceID string) error {
	if strings.TrimSpace(workspaceID) == "" {
		return fmt.Errorf("%w: workspace_id is required", runtimetypes.ErrEventMissingRequiredField)
	}
	return nil
}

package runtimetypes

import (
	"context"
	"fmt"
	"time"

	libdb "github.com/contenox/contenox/libdbexec"
	"github.com/contenox/contenox/libevents"
)

var eventStoreConfig = libevents.Config{TablePrefix: "event_", ScopeColumn: "workspace_id"}

// Firing statuses recorded on event_firings rows.
const (
	EventFiringStatusRunning = libevents.FiringStatusRunning
	EventFiringStatusOK      = libevents.FiringStatusOK
	EventFiringStatusError   = libevents.FiringStatusError
	EventFiringStatusRefused = libevents.FiringStatusRefused
)

// Firing-listing bounds: an unnamed limit gets DefaultEventFiringLimit, and
// anything above MaxEventFiringLimit is clamped to it.
const (
	DefaultEventFiringLimit = libevents.DefaultFiringLimit
	MaxEventFiringLimit     = libevents.MaxFiringLimit
)

// StaleEventFiringClaim bounds how long a running claim keeps a (trigger,
// event) pair from being retried before the next BeginEventFiring takes it over.
const StaleEventFiringClaim = 2 * time.Hour

// EventFiring is one recorded (trigger, event) execution attempt.
type EventFiring struct {
	WorkspaceID string
	TriggerName string
	NID         int64
	Status      string
	Error       string
	RequestID   string
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// Stranded reports whether f is a running claim no live host can still hold at now.
func (f EventFiring) Stranded(now time.Time) bool {
	return libevents.Firing{Status: f.Status, UpdatedAt: f.UpdatedAt}.Stranded(now, StaleEventFiringClaim)
}

// EventFiringFilter narrows a ListEventFirings read to one workspace; every
// field is optional, and the zero value lists the whole workspace newest-first
// up to DefaultEventFiringLimit.
type EventFiringFilter struct {
	// SinceNID keeps firings whose event nid is greater than it; 0 keeps all.
	SinceNID int64
	// Status keeps one EventFiringStatus* value; "" keeps all.
	Status string
	// TriggerName keeps one trigger's firings; "" keeps all.
	TriggerName string
	// Limit caps the page: <= 0 means DefaultEventFiringLimit, more than
	// MaxEventFiringLimit is clamped to it.
	Limit int
}

// EventFiringStore is the dispatcher's durable state, scoped to one workspace
// at construction.
type EventFiringStore interface {
	// GetEventCursor returns consumer's last processed NID, 0 when none exists.
	GetEventCursor(ctx context.Context, consumer string) (int64, error)
	// SetEventCursor upserts consumer's cursor to nid.
	SetEventCursor(ctx context.Context, consumer string, nid int64) error
	// BeginEventFiring claims (triggerName, nid) via a conflict-ignoring INSERT,
	// returning false when already claimed. A stale running claim is reclaimable.
	BeginEventFiring(ctx context.Context, triggerName string, nid int64, requestID string) (bool, error)
	// FinishEventFiring records the outcome of a claimed firing.
	FinishEventFiring(ctx context.Context, triggerName string, nid int64, status, errMsg string) error
	// ListEventFirings returns the workspace's firings matching f, newest
	// first; read-only, it never appends an event.
	ListEventFirings(ctx context.Context, f EventFiringFilter) ([]EventFiring, error)
}

type eventFiringStore struct {
	exec    libdb.Exec
	cursors *libevents.CursorStore
	firings *libevents.FiringStore
}

// EventFiringStoreOption configures a firing store at construction.
type EventFiringStoreOption func(*eventFiringStoreOptions)

type eventFiringStoreOptions struct {
	now func() time.Time
}

// WithEventFiringClock overrides the store's time source, letting a test age
// a claim past StaleEventFiringClaim instead of sleeping.
func WithEventFiringClock(now func() time.Time) EventFiringStoreOption {
	return func(o *eventFiringStoreOptions) {
		if now != nil {
			o.now = now
		}
	}
}

// NewEventFiringStore creates a dispatcher store bound to exec, scoped to
// workspaceID.
func NewEventFiringStore(exec libdb.Exec, workspaceID string, opts ...EventFiringStoreOption) (EventFiringStore, error) {
	if exec == nil {
		return nil, fmt.Errorf("runtimetypes: event firing store requires an exec")
	}
	o := eventFiringStoreOptions{now: time.Now}
	for _, opt := range opts {
		opt(&o)
	}
	cursors, err := libevents.NewCursorStore(eventStoreConfig, workspaceID)
	if err != nil {
		return nil, fmt.Errorf("runtimetypes: event cursor store: %w", err)
	}
	firings, err := libevents.NewFiringStore(eventStoreConfig, workspaceID, StaleEventFiringClaim, libevents.WithClock(o.now))
	if err != nil {
		return nil, fmt.Errorf("runtimetypes: event firing store: %w", err)
	}
	return &eventFiringStore{exec: exec, cursors: cursors, firings: firings}, nil
}

func (s *eventFiringStore) GetEventCursor(ctx context.Context, consumer string) (int64, error) {
	return s.cursors.GetCursor(ctx, s.exec, consumer)
}

func (s *eventFiringStore) SetEventCursor(ctx context.Context, consumer string, nid int64) error {
	return s.cursors.SetCursor(ctx, s.exec, consumer, nid)
}

func (s *eventFiringStore) BeginEventFiring(ctx context.Context, triggerName string, nid int64, requestID string) (bool, error) {
	return s.firings.BeginFiring(ctx, s.exec, triggerName, nid, requestID)
}

func (s *eventFiringStore) FinishEventFiring(ctx context.Context, triggerName string, nid int64, status, errMsg string) error {
	return s.firings.FinishFiring(ctx, s.exec, triggerName, nid, status, errMsg)
}

func (s *eventFiringStore) ListEventFirings(ctx context.Context, f EventFiringFilter) ([]EventFiring, error) {
	firings, err := s.firings.ListFirings(ctx, s.exec, libevents.FiringFilter{
		SinceNID:    f.SinceNID,
		Status:      f.Status,
		TriggerName: f.TriggerName,
		Limit:       f.Limit,
	})
	if err != nil {
		return nil, err
	}
	out := make([]EventFiring, 0, len(firings))
	for _, fr := range firings {
		out = append(out, EventFiring{
			WorkspaceID: fr.Scope,
			TriggerName: fr.TriggerName,
			NID:         fr.NID,
			Status:      fr.Status,
			Error:       fr.Error,
			RequestID:   fr.RequestID,
			CreatedAt:   fr.CreatedAt,
			UpdatedAt:   fr.UpdatedAt,
		})
	}
	return out, nil
}

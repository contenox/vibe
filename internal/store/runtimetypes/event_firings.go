package runtimetypes

import (
	"context"
	"fmt"
	"time"

	libdb "github.com/contenox/contenox/libdbexec"
	"github.com/contenox/contenox/libevents"
)

// Dispatcher-side durable state over event_cursors and event_firings: where a
// named consumer has read to, and which (trigger, event) pairs have already
// fired. Both are WORKSPACE-scoped by construction, so two workspaces'
// dispatchers never share a cursor or a firing claim. The mechanism lives in
// libevents; this file binds it to the runtime's tables and workspace scope
// so external importers and this tree compile against one definition of the
// claim, the takeover, and the outcome row.

// eventStoreConfig names the runtime's tables for libevents: the same
// event_cursors / event_firings shapes this mechanism was extracted from.
var eventStoreConfig = libevents.Config{TablePrefix: "event_", ScopeColumn: "workspace_id"}

// Firing statuses recorded on event_firings rows.
const (
	EventFiringStatusRunning = libevents.FiringStatusRunning
	EventFiringStatusOK      = libevents.FiringStatusOK
	EventFiringStatusError   = libevents.FiringStatusError
	EventFiringStatusRefused = libevents.FiringStatusRefused
)

// Firing-listing bounds. A caller that names no limit gets
// DefaultEventFiringLimit; one that asks for more than MaxEventFiringLimit is
// clamped.
const (
	DefaultEventFiringLimit = libevents.DefaultFiringLimit
	MaxEventFiringLimit     = libevents.MaxFiringLimit
)

// StaleEventFiringClaim bounds how long a running claim keeps a (trigger,
// event) pair from being retried: a claim untouched for longer is taken over
// by the next BeginEventFiring, so a firing whose host died mid-run is
// retried rather than lost.
//
// Derivation. This table's sibling claim — chain_checkpoints, reclaimed after
// agentservice's resumeClaimStaleness of 10 minutes — measures SILENCE: its
// live holder heartbeats at a fifth of that (TouchChainCheckpointClaim), so
// the bound need only exceed a missed heartbeat. A firing claim has no
// heartbeat. The row is written once before the chain runs and once after it
// ends, so staleness here measures the WHOLE RUN, and the bound must exceed
// the longest legitimate one. This tree's hard ceiling on a single agent turn
// is one hour (nativeturn.DefaultTurnDeadline — named, not imported: the
// store sits below the kernel), and the shortest useful shape of a fired
// chain is judgment plus actuation, two turns: 2 × 1h is the first bound no
// live firing can reach. Overshooting costs only how long a dead host's
// firing waits for its retry; undershooting costs the tier's other half — a
// slow but living firing stolen and executed twice. The bound must be
// re-derived whenever the turn deadline moves, which is why libevents takes
// it as a required parameter instead of exporting a constant.
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

// Stranded reports whether f is a running claim no live host can still hold
// at now — the operator-visible half of StaleEventFiringClaim. A stranded
// firing has not failed and has not succeeded: its host died between the
// claim and the outcome, so nothing was ever recorded about it.
func (f EventFiring) Stranded(now time.Time) bool {
	return libevents.Firing{Status: f.Status, UpdatedAt: f.UpdatedAt}.Stranded(now, StaleEventFiringClaim)
}

// EventFiringFilter narrows a ListEventFirings read. Every field is optional:
// the zero value lists the store's whole workspace, newest first, up to
// DefaultEventFiringLimit. The workspace is never a filter field — it is
// fixed by the store, so no filter can widen a read past its own workspace.
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
	// BeginEventFiring claims (triggerName, nid) with status running. Returns
	// false when the pair is already claimed — the at-least-once dedup. The
	// claim is one conflict-ignoring INSERT against the primary key, never a
	// select-then-insert: at-most-once is the PK's guarantee, not a race the
	// caller wins.
	//
	// A claim already held is reclaimable in exactly one case: a running row
	// untouched for StaleEventFiringClaim, whose host died before it could
	// record an outcome. That takeover is a second conditional UPDATE, equally
	// structural — the freshness predicate is re-evaluated under the row's
	// write lock, so of two racing hosts only one can observe it true.
	BeginEventFiring(ctx context.Context, triggerName string, nid int64, requestID string) (bool, error)
	// FinishEventFiring records the outcome of a claimed firing.
	FinishEventFiring(ctx context.Context, triggerName string, nid int64, status, errMsg string) error
	// ListEventFirings returns the workspace's firings matching f, newest
	// first. Read-only: the observability path never appends an event.
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

// WithEventFiringClock overrides the store's time source — the same seam
// WithEventClock gives the event log, so a test can age a claim past
// StaleEventFiringClaim instead of sleeping through it.
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

package runtimetypes

import (
	"context"
	"fmt"
	"time"

	libdb "github.com/contenox/contenox/libdbexec"
)

// Dispatcher-side durable state over event_cursors and event_firings: where a
// named consumer has read to, and which (trigger, event) pairs have already
// fired. Both are WORKSPACE-scoped by construction, so two workspaces'
// dispatchers never share a cursor or a firing claim.

// Firing statuses recorded on event_firings rows.
const (
	EventFiringStatusRunning = "running"
	EventFiringStatusOK      = "ok"
	EventFiringStatusError   = "error"
	EventFiringStatusRefused = "refused"
)

// Firing-listing bounds. A caller that names no limit gets
// DefaultEventFiringLimit; one that asks for more than MaxEventFiringLimit is
// clamped.
const (
	DefaultEventFiringLimit = 50
	MaxEventFiringLimit     = 1000
)

// StaleEventFiringClaim bounds how long a running claim keeps a (trigger,
// event) pair from being retried: a claim untouched for longer is taken over by
// the next BeginEventFiring, so a firing whose host died mid-run is retried
// rather than lost.
//
// Derivation. This table's sibling claim — chain_checkpoints, reclaimed after
// agentservice's resumeClaimStaleness of 10 minutes — measures SILENCE: its
// live holder heartbeats at a fifth of that (TouchChainCheckpointClaim), so the
// bound need only exceed a missed heartbeat. A firing claim has no heartbeat.
// The row is written once before the chain runs and once after it ends, so
// staleness here measures the WHOLE RUN, and the bound must exceed the longest
// legitimate one. This tree's hard ceiling on a single agent turn is 15 minutes
// (nativeturn.DefaultTurnDeadline — named, not imported: the store sits below
// the kernel), and the shortest useful shape of a fired chain is judgment plus
// actuation, two turns: 2 × 15m = 30m is the first bound no live firing can
// reach. Overshooting costs only how long a dead host's firing waits for its
// retry; undershooting costs the tier's other half — a slow but living firing
// stolen and executed twice.
const StaleEventFiringClaim = 30 * time.Minute

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

// Stranded reports whether f is a running claim no live host can still hold at
// now — the operator-visible half of StaleEventFiringClaim. A stranded firing
// has not failed and has not succeeded: its host died between the claim and the
// outcome, so nothing was ever recorded about it.
func (f EventFiring) Stranded(now time.Time) bool {
	return f.Status == EventFiringStatusRunning && now.UTC().Sub(f.UpdatedAt) > StaleEventFiringClaim
}

// EventFiringFilter narrows a ListEventFirings read. Every field is optional:
// the zero value lists the store's whole workspace, newest first, up to
// DefaultEventFiringLimit. The workspace is never a filter field — it is fixed
// by the store, so no filter can widen a read past its own workspace.
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
	// claim is ONE INSERT OR IGNORE against the primary key, never a
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
	exec        libdb.Exec
	workspaceID string
	now         func() time.Time
}

// EventFiringStoreOption configures a firing store at construction.
type EventFiringStoreOption func(*eventFiringStore)

// WithEventFiringClock overrides the store's time source — the same seam
// WithEventClock gives the event log, so a test can age a claim past
// StaleEventFiringClaim instead of sleeping through it.
func WithEventFiringClock(now func() time.Time) EventFiringStoreOption {
	return func(s *eventFiringStore) {
		if now != nil {
			s.now = now
		}
	}
}

// NewEventFiringStore creates a dispatcher store bound to exec, scoped to
// workspaceID.
func NewEventFiringStore(exec libdb.Exec, workspaceID string, opts ...EventFiringStoreOption) EventFiringStore {
	if exec == nil {
		panic("SERVER BUG: runtimetypes.NewEventFiringStore called with nil exec")
	}
	s := &eventFiringStore{exec: exec, workspaceID: workspaceID, now: time.Now}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

func (s *eventFiringStore) GetEventCursor(ctx context.Context, consumer string) (int64, error) {
	rows, err := s.exec.QueryContext(ctx,
		`SELECT last_nid FROM event_cursors WHERE consumer = $1 AND workspace_id = $2`,
		consumer, s.workspaceID)
	if err != nil {
		return 0, fmt.Errorf("event_cursors: get %q: %w", consumer, err)
	}
	defer rows.Close()
	var nid int64
	if rows.Next() {
		if err := rows.Scan(&nid); err != nil {
			return 0, fmt.Errorf("event_cursors: scan row: %w", err)
		}
	}
	return nid, rows.Err()
}

func (s *eventFiringStore) SetEventCursor(ctx context.Context, consumer string, nid int64) error {
	_, err := s.exec.ExecContext(ctx, `
		INSERT INTO event_cursors (consumer, workspace_id, last_nid, updated_at)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (consumer, workspace_id) DO UPDATE SET last_nid = $3, updated_at = $4
	`, consumer, s.workspaceID, nid, s.now().UTC())
	if err != nil {
		return fmt.Errorf("event_cursors: set %q: %w", consumer, err)
	}
	return nil
}

func (s *eventFiringStore) BeginEventFiring(ctx context.Context, triggerName string, nid int64, requestID string) (bool, error) {
	now := s.now().UTC()
	res, err := s.exec.ExecContext(ctx, `
		INSERT OR IGNORE INTO event_firings (workspace_id, trigger_name, nid, status, request_id, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $6)
	`, s.workspaceID, triggerName, nid, EventFiringStatusRunning, requestID, now)
	if err != nil {
		return false, fmt.Errorf("event_firings: begin %q/%d: %w", triggerName, nid, err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	if affected == 1 {
		return true, nil
	}
	return s.reclaimEventFiring(ctx, triggerName, nid, requestID, now)
}

// reclaimEventFiring takes over a running claim untouched since
// StaleEventFiringClaim — the row a host left behind when it died between the
// claim and the outcome. created_at is deliberately kept: it dates the first
// attempt, so the retry is visible as a retry rather than as a fresh firing.
// Every other already-claimed row (ok, error, refused, or a running claim still
// inside the bound) fails the predicate and stays claimed, which is the
// at-most-once half of the contract.
func (s *eventFiringStore) reclaimEventFiring(ctx context.Context, triggerName string, nid int64, requestID string, now time.Time) (bool, error) {
	res, err := s.exec.ExecContext(ctx, `
		UPDATE event_firings SET request_id = $4, error = '', updated_at = $5
		WHERE workspace_id = $1 AND trigger_name = $2 AND nid = $3
		  AND status = $6 AND updated_at < $7
	`, s.workspaceID, triggerName, nid, requestID, now, EventFiringStatusRunning, now.Add(-StaleEventFiringClaim))
	if err != nil {
		return false, fmt.Errorf("event_firings: reclaim %q/%d: %w", triggerName, nid, err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return affected == 1, nil
}

func (s *eventFiringStore) FinishEventFiring(ctx context.Context, triggerName string, nid int64, status, errMsg string) error {
	_, err := s.exec.ExecContext(ctx, `
		UPDATE event_firings SET status = $4, error = $5, updated_at = $6
		WHERE workspace_id = $1 AND trigger_name = $2 AND nid = $3
	`, s.workspaceID, triggerName, nid, status, errMsg, s.now().UTC())
	if err != nil {
		return fmt.Errorf("event_firings: finish %q/%d: %w", triggerName, nid, err)
	}
	return nil
}

func (s *eventFiringStore) ListEventFirings(ctx context.Context, f EventFiringFilter) ([]EventFiring, error) {
	limit := f.Limit
	if limit <= 0 {
		limit = DefaultEventFiringLimit
	}
	if limit > MaxEventFiringLimit {
		limit = MaxEventFiringLimit
	}
	// The workspace predicate is placeholder $1 and unconditional; optional
	// predicates append in call order so $N always matches len(args).
	args := []any{s.workspaceID}
	where := "workspace_id = $1"
	if f.SinceNID > 0 {
		args = append(args, f.SinceNID)
		where += fmt.Sprintf(" AND nid > $%d", len(args))
	}
	if f.Status != "" {
		args = append(args, f.Status)
		where += fmt.Sprintf(" AND status = $%d", len(args))
	}
	if f.TriggerName != "" {
		args = append(args, f.TriggerName)
		where += fmt.Sprintf(" AND trigger_name = $%d", len(args))
	}
	args = append(args, limit)
	rows, err := s.exec.QueryContext(ctx, fmt.Sprintf(`
		SELECT workspace_id, trigger_name, nid, status, COALESCE(error, ''), request_id, created_at, updated_at
		FROM event_firings
		WHERE %s
		ORDER BY nid DESC, trigger_name
		LIMIT $%d
	`, where, len(args)), args...)
	if err != nil {
		return nil, fmt.Errorf("event_firings: list: %w", err)
	}
	defer rows.Close()
	firings := []EventFiring{}
	for rows.Next() {
		var f EventFiring
		if err := rows.Scan(&f.WorkspaceID, &f.TriggerName, &f.NID, &f.Status, &f.Error, &f.RequestID, &f.CreatedAt, &f.UpdatedAt); err != nil {
			return nil, fmt.Errorf("event_firings: scan row: %w", err)
		}
		firings = append(firings, f)
	}
	return firings, rows.Err()
}

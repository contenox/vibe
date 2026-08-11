package libevents

import (
	"context"
	"fmt"
	"time"

	libdb "github.com/contenox/contenox/libdbexec"
)

// Firing statuses recorded on firing rows.
const (
	FiringStatusRunning = "running"
	FiringStatusOK      = "ok"
	FiringStatusError   = "error"
	FiringStatusRefused = "refused"
)

// Firing-listing bounds. A caller that names no limit gets
// DefaultFiringLimit; one that asks for more than MaxFiringLimit is clamped.
const (
	DefaultFiringLimit = 50
	MaxFiringLimit     = 1000
)

// Firing is one recorded (trigger, event) execution attempt.
type Firing struct {
	Scope       string
	TriggerName string
	NID         int64
	Status      string
	Error       string
	RequestID   string
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// Stranded reports whether f is a running claim no live host can still hold
// at now, given the store's stale-claim bound — the operator-visible state
// between "running" and any outcome. A stranded firing's host died between
// the claim and the outcome, so nothing was ever recorded about it.
func (f Firing) Stranded(now time.Time, staleClaim time.Duration) bool {
	return f.Status == FiringStatusRunning && now.UTC().Sub(f.UpdatedAt) > staleClaim
}

// FiringFilter narrows a ListFirings read. Every field is optional: the zero
// value lists the store's whole scope, newest first, up to
// DefaultFiringLimit. The scope is never a filter field — it is fixed by the
// store, so no filter can widen a read past it.
type FiringFilter struct {
	// SinceNID keeps firings whose event nid is greater than it; 0 keeps all.
	SinceNID int64
	// Status keeps one FiringStatus* value; "" keeps all.
	Status string
	// TriggerName keeps one trigger's firings; "" keeps all.
	TriggerName string
	// Limit caps the page: <= 0 means DefaultFiringLimit, more than
	// MaxFiringLimit is clamped to it.
	Limit int
}

// FiringStore is the durable record of which (trigger, event) pairs ran and
// how they ended, scoped at construction.
//
// A claim held inside a caller's transaction releases itself on rollback —
// the claim style for effects that are themselves database writes, which
// need no staleness machinery at all. The stale-claim bound exists for the
// other style: a claim committed before work that runs outside any
// transaction, whose host can die holding it.
type FiringStore struct {
	cfg        Config
	scope      string
	staleClaim time.Duration
	now        func() time.Time
}

// FiringStoreOption configures a firing store at construction.
type FiringStoreOption func(*FiringStore)

// WithClock overrides the store's time source, so a test can age a claim
// past the stale bound instead of sleeping through it.
func WithClock(now func() time.Time) FiringStoreOption {
	return func(s *FiringStore) {
		if now != nil {
			s.now = now
		}
	}
}

// NewFiringStore builds a firing store for one scope. staleClaim bounds how
// long a running claim keeps its (trigger, event) pair from being retried,
// and is required with no default: it must be derived from the caller's own
// longest legitimate run — an SMTP send's timeout, an agent chain's turn
// ceiling — because a bound copied from another workload silently rots when
// that workload changes. Overshooting costs only how long a dead host's
// firing waits for its retry; undershooting steals a slow but living firing
// and executes it twice.
func NewFiringStore(cfg Config, scope string, staleClaim time.Duration, opts ...FiringStoreOption) (*FiringStore, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if staleClaim <= 0 {
		return nil, fmt.Errorf("libevents: stale-claim bound must be positive, got %v", staleClaim)
	}
	s := &FiringStore{cfg: cfg, scope: scope, staleClaim: staleClaim, now: time.Now}
	for _, opt := range opts {
		opt(s)
	}
	return s, nil
}

// StaleClaim returns the bound the store was constructed with, for callers
// that compute Stranded over listed firings.
func (s *FiringStore) StaleClaim() time.Duration { return s.staleClaim }

// BeginFiring claims (triggerName, nid) with status running. Returns false
// when the pair is already claimed — the at-least-once dedup. The claim is
// one conflict-ignoring INSERT against the primary key, never a
// select-then-insert: at-most-once is the primary key's guarantee, not a
// race the caller wins.
//
// A claim already held is reclaimable in exactly one case: a running row
// untouched past the store's stale-claim bound, whose host died before it
// could record an outcome. That takeover is a second conditional UPDATE,
// equally structural — the freshness predicate is re-evaluated under the
// row's write lock, so of two racing hosts only one can observe it true.
func (s *FiringStore) BeginFiring(ctx context.Context, exec libdb.Exec, triggerName string, nid int64, requestID string) (bool, error) {
	now := s.now().UTC()
	res, err := exec.ExecContext(ctx, fmt.Sprintf(`
		INSERT INTO %s (%s, trigger_name, nid, status, error, request_id, created_at, updated_at)
		VALUES ($1, $2, $3, $4, '', $5, $6, $6)
		ON CONFLICT (%s, trigger_name, nid) DO NOTHING`,
		s.cfg.table("firings"), s.cfg.ScopeColumn, s.cfg.ScopeColumn),
		s.scope, triggerName, nid, FiringStatusRunning, requestID, now)
	if err != nil {
		return false, fmt.Errorf("libevents: begin firing %q/%d: %w", triggerName, nid, err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	if affected == 1 {
		return true, nil
	}
	return s.reclaimFiring(ctx, exec, triggerName, nid, requestID, now)
}

// reclaimFiring takes over a running claim untouched past the stale bound.
// created_at is deliberately kept: it dates the first attempt, so the retry
// is visible as a retry rather than as a fresh firing. Every other
// already-claimed row fails the predicate and stays claimed.
func (s *FiringStore) reclaimFiring(ctx context.Context, exec libdb.Exec, triggerName string, nid int64, requestID string, now time.Time) (bool, error) {
	res, err := exec.ExecContext(ctx, fmt.Sprintf(`
		UPDATE %s SET request_id = $4, error = '', updated_at = $5
		WHERE %s = $1 AND trigger_name = $2 AND nid = $3
		  AND status = $6 AND updated_at < $7`,
		s.cfg.table("firings"), s.cfg.ScopeColumn),
		s.scope, triggerName, nid, requestID, now, FiringStatusRunning, now.Add(-s.staleClaim))
	if err != nil {
		return false, fmt.Errorf("libevents: reclaim firing %q/%d: %w", triggerName, nid, err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return affected == 1, nil
}

// FinishFiring records the outcome of a claimed firing.
func (s *FiringStore) FinishFiring(ctx context.Context, exec libdb.Exec, triggerName string, nid int64, status, errMsg string) error {
	_, err := exec.ExecContext(ctx, fmt.Sprintf(`
		UPDATE %s SET status = $4, error = $5, updated_at = $6
		WHERE %s = $1 AND trigger_name = $2 AND nid = $3`,
		s.cfg.table("firings"), s.cfg.ScopeColumn),
		s.scope, triggerName, nid, status, errMsg, s.now().UTC())
	if err != nil {
		return fmt.Errorf("libevents: finish firing %q/%d: %w", triggerName, nid, err)
	}
	return nil
}

// ResetFiring is the operator's retry verb: it turns one settled firing back
// into a running row whose updated_at is backdated past the stale bound, so
// the next BeginFiring reclaims it immediately while created_at keeps dating
// the first attempt. It refuses to touch a running row still inside the
// bound, so a live run cannot be stolen by an impatient retry.
func (s *FiringStore) ResetFiring(ctx context.Context, exec libdb.Exec, triggerName string, nid int64) (bool, error) {
	now := s.now().UTC()
	res, err := exec.ExecContext(ctx, fmt.Sprintf(`
		UPDATE %s SET status = $4, error = '', updated_at = $5
		WHERE %s = $1 AND trigger_name = $2 AND nid = $3
		  AND (status != $4 OR updated_at < $6)`,
		s.cfg.table("firings"), s.cfg.ScopeColumn),
		s.scope, triggerName, nid, FiringStatusRunning,
		now.Add(-s.staleClaim-time.Second), now.Add(-s.staleClaim))
	if err != nil {
		return false, fmt.Errorf("libevents: reset firing %q/%d: %w", triggerName, nid, err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return affected == 1, nil
}

// ListFirings returns the scope's firings matching f, newest first.
// Read-only: nothing that observes firings may append an event, or an
// incident amplifies itself.
func (s *FiringStore) ListFirings(ctx context.Context, exec libdb.Exec, f FiringFilter) ([]Firing, error) {
	limit := f.Limit
	if limit <= 0 {
		limit = DefaultFiringLimit
	}
	if limit > MaxFiringLimit {
		limit = MaxFiringLimit
	}
	args := []any{s.scope}
	where := s.cfg.ScopeColumn + " = $1"
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
	rows, err := exec.QueryContext(ctx, fmt.Sprintf(`
		SELECT %s, trigger_name, nid, status, COALESCE(error, ''), request_id, created_at, updated_at
		FROM %s
		WHERE %s
		ORDER BY nid DESC, trigger_name
		LIMIT $%d`,
		s.cfg.ScopeColumn, s.cfg.table("firings"), where, len(args)), args...)
	if err != nil {
		return nil, fmt.Errorf("libevents: list firings: %w", err)
	}
	defer rows.Close()
	firings := []Firing{}
	for rows.Next() {
		var out Firing
		if err := rows.Scan(&out.Scope, &out.TriggerName, &out.NID, &out.Status, &out.Error, &out.RequestID, &out.CreatedAt, &out.UpdatedAt); err != nil {
			return nil, fmt.Errorf("libevents: scan firing: %w", err)
		}
		firings = append(firings, out)
	}
	return firings, rows.Err()
}

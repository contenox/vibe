package libevents

import (
	"context"
	"fmt"
	"time"

	libdb "github.com/contenox/contenox/libdbexec"
)

// Staging bounds.
const (
	DefaultStagingLimit = 100
	MaxStagingLimit     = 1000
)

// StagedEvent is an event payload held back until DelayedUntil.
type StagedEvent struct {
	ID           string
	Scope        string
	Payload      []byte
	DelayedUntil time.Time
	CreatedAt    time.Time
}

// StagingStore holds staged events for one scope, fixed at construction.
type StagingStore struct {
	cfg   Config
	scope string
	now   func() time.Time
}

// NewStagingStore builds a staging store for one scope.
func NewStagingStore(cfg Config, scope string) (*StagingStore, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &StagingStore{cfg: cfg, scope: scope, now: time.Now}, nil
}

// AppendStagedEvent stores e in the caller's transaction; ID and Payload are
// required, and a zero DelayedUntil means due immediately.
func (s *StagingStore) AppendStagedEvent(ctx context.Context, exec libdb.Exec, e *StagedEvent) error {
	if e.ID == "" {
		return fmt.Errorf("libevents: staged event requires an id")
	}
	if len(e.Payload) == 0 {
		return fmt.Errorf("libevents: staged event %q carries no payload", e.ID)
	}
	now := s.now().UTC()
	due := e.DelayedUntil
	if due.IsZero() {
		due = now
	}
	_, err := exec.ExecContext(ctx, fmt.Sprintf(`
		INSERT INTO %s (id, %s, payload, delayed_until, created_at)
		VALUES ($1, $2, $3, $4, $5)`,
		s.cfg.table("staging"), s.cfg.ScopeColumn),
		e.ID, s.scope, string(e.Payload), due.UTC(), now)
	if err != nil {
		return fmt.Errorf("libevents: append staged event %q: %w", e.ID, err)
	}
	e.Scope, e.DelayedUntil, e.CreatedAt = s.scope, due.UTC(), now
	return nil
}

// ListDueStagedEvents returns staged events due at now, oldest first, up to
// limit; callers delete what they drain via DeleteStagedEvents in the same
// transaction.
func (s *StagingStore) ListDueStagedEvents(ctx context.Context, exec libdb.Exec, now time.Time, limit int) ([]*StagedEvent, error) {
	if limit <= 0 {
		limit = DefaultStagingLimit
	}
	if limit > MaxStagingLimit {
		limit = MaxStagingLimit
	}
	rows, err := exec.QueryContext(ctx, fmt.Sprintf(`
		SELECT id, %s, payload, delayed_until, created_at
		FROM %s WHERE %s = $1 AND delayed_until <= $2
		ORDER BY delayed_until, id LIMIT $3`,
		s.cfg.ScopeColumn, s.cfg.table("staging"), s.cfg.ScopeColumn),
		s.scope, now.UTC(), limit)
	if err != nil {
		return nil, fmt.Errorf("libevents: list due staged events: %w", err)
	}
	defer rows.Close()
	out := []*StagedEvent{}
	for rows.Next() {
		var e StagedEvent
		var payload string
		if err := rows.Scan(&e.ID, &e.Scope, &payload, &e.DelayedUntil, &e.CreatedAt); err != nil {
			return nil, fmt.Errorf("libevents: scan staged event: %w", err)
		}
		e.Payload = []byte(payload)
		out = append(out, &e)
	}
	return out, rows.Err()
}

// DeleteStagedEvents removes the named staged rows in the caller's
// transaction.
func (s *StagingStore) DeleteStagedEvents(ctx context.Context, exec libdb.Exec, ids ...string) error {
	for _, id := range ids {
		if _, err := exec.ExecContext(ctx, fmt.Sprintf(
			`DELETE FROM %s WHERE %s = $1 AND id = $2`,
			s.cfg.table("staging"), s.cfg.ScopeColumn), s.scope, id); err != nil {
			return fmt.Errorf("libevents: delete staged event %q: %w", id, err)
		}
	}
	return nil
}

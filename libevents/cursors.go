package libevents

import (
	"context"
	"fmt"
	"time"

	libdb "github.com/contenox/contenox/libdbexec"
)

// CursorStore is a named consumer's durable position over an event log,
// scoped at construction. Rewinding a cursor is the replay verb: a consumer
// restarted behind its cursor re-reads exactly what it had not settled.
type CursorStore struct {
	cfg   Config
	scope string
	now   func() time.Time
}

// NewCursorStore builds a cursor store for one scope. The empty scope is a
// valid scope of its own, for importers whose consumers read cross-scope.
func NewCursorStore(cfg Config, scope string) (*CursorStore, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &CursorStore{cfg: cfg, scope: scope, now: time.Now}, nil
}

// GetCursor returns consumer's last settled NID, 0 when none exists.
func (s *CursorStore) GetCursor(ctx context.Context, exec libdb.Exec, consumer string) (int64, error) {
	rows, err := exec.QueryContext(ctx, fmt.Sprintf(
		`SELECT last_nid FROM %s WHERE consumer = $1 AND %s = $2`,
		s.cfg.table("cursors"), s.cfg.ScopeColumn), consumer, s.scope)
	if err != nil {
		return 0, fmt.Errorf("libevents: get cursor %q: %w", consumer, err)
	}
	defer rows.Close()
	var nid int64
	if rows.Next() {
		if err := rows.Scan(&nid); err != nil {
			return 0, fmt.Errorf("libevents: scan cursor: %w", err)
		}
	}
	return nid, rows.Err()
}

// SetCursor upserts consumer's position to nid. Setting a lower value than
// the stored one is permitted deliberately: that is the rewind.
func (s *CursorStore) SetCursor(ctx context.Context, exec libdb.Exec, consumer string, nid int64) error {
	_, err := exec.ExecContext(ctx, fmt.Sprintf(`
		INSERT INTO %s (consumer, %s, last_nid, updated_at)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (consumer, %s) DO UPDATE SET last_nid = $3, updated_at = $4`,
		s.cfg.table("cursors"), s.cfg.ScopeColumn, s.cfg.ScopeColumn),
		consumer, s.scope, nid, s.now().UTC())
	if err != nil {
		return fmt.Errorf("libevents: set cursor %q: %w", consumer, err)
	}
	return nil
}

package libevents

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	libdb "github.com/contenox/contenox/libdbexec"
)

// Listener kinds: what an arriving event does to the target.
const (
	// ListenerKindStart starts new work at the target.
	ListenerKindStart = "start"
	// ListenerKindWake resumes something already in flight that registered
	// this listener and is waiting on it.
	ListenerKindWake = "wake"
)

// Listener-listing bounds, shared with the firing limits' rationale.
const (
	DefaultListenerLimit = 50
	MaxListenerLimit     = 1000
)

// Listener is one durable subscription: which event types, filtered how,
// doing what to which target. It is a row rather than configuration because
// waits are registered and consumed at runtime — a waiting consumer creates
// its listener mid-run and the firing that wakes it deletes it in the same
// transaction.
type Listener struct {
	ID    string
	Scope string
	// Kind is ListenerKindStart or ListenerKindWake.
	Kind string
	// Target is opaque to this package: a chain reference, a delivery
	// address, an instance to wake. The dispatcher that owns the listener
	// interprets it.
	Target string
	// Owner keys bulk cleanup: every listener registered by one instance,
	// session, or configuration unit dies with it via DeleteListenersByOwner.
	Owner string
	// OneShot listeners are deleted by their consumer when they fire; the
	// store carries the flag, the dispatcher enforces it.
	OneShot bool
	// Types are the exact event types subscribed. Each becomes a topic row,
	// so fan-out is an indexed lookup rather than a scan of all listeners.
	Types []string
	// ContextFilters narrows matching beyond the type: per event type, a map
	// of event context attribute to glob pattern, every entry of which must
	// match. Correlation — this session's event, not any event of the type —
	// lives here, as data on the subscription.
	ContextFilters map[string]map[string]string
	// Metadata is opaque JSON for the importer's extensions.
	Metadata  string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// ListenerStore is the durable subscription registry, scoped at construction.
// Mutators take the caller's Exec so a registration or a one-shot consumption
// shares the transaction of the work that caused it.
type ListenerStore struct {
	cfg   Config
	scope string
	now   func() time.Time
}

// NewListenerStore builds a listener store for one scope.
func NewListenerStore(cfg Config, scope string) (*ListenerStore, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &ListenerStore{cfg: cfg, scope: scope, now: time.Now}, nil
}

// AppendListener stores l and its topic rows in the caller's transaction.
// l.ID and at least one type are required; the scope is the store's own.
func (s *ListenerStore) AppendListener(ctx context.Context, exec libdb.Exec, l *Listener) error {
	if l.ID == "" {
		return fmt.Errorf("libevents: listener requires an id")
	}
	if len(l.Types) == 0 {
		return fmt.Errorf("libevents: listener %q subscribes to no types", l.ID)
	}
	if l.Kind != ListenerKindStart && l.Kind != ListenerKindWake {
		return fmt.Errorf("libevents: unknown listener kind %q", l.Kind)
	}
	filters, err := json.Marshal(l.ContextFilters)
	if err != nil {
		return fmt.Errorf("libevents: encode context filters: %w", err)
	}
	now := s.now().UTC()
	_, err = exec.ExecContext(ctx, fmt.Sprintf(`
		INSERT INTO %s (id, %s, kind, target, owner, one_shot, context_filters, metadata, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $9)`,
		s.cfg.table("listeners"), s.cfg.ScopeColumn),
		l.ID, s.scope, l.Kind, l.Target, l.Owner, l.OneShot, string(filters), l.Metadata, now)
	if err != nil {
		return fmt.Errorf("libevents: append listener %q: %w", l.ID, err)
	}
	for _, t := range l.Types {
		if _, err := exec.ExecContext(ctx, fmt.Sprintf(`
			INSERT INTO %s (%s, event_type, listener_id) VALUES ($1, $2, $3)
			ON CONFLICT (%s, event_type, listener_id) DO NOTHING`,
			s.cfg.table("listener_topics"), s.cfg.ScopeColumn, s.cfg.ScopeColumn),
			s.scope, t, l.ID); err != nil {
			return fmt.Errorf("libevents: append topic %q for %q: %w", t, l.ID, err)
		}
	}
	l.Scope, l.CreatedAt, l.UpdatedAt = s.scope, now, now
	return nil
}

// GetListener returns one listener by id, or libdbexec.ErrNotFound.
func (s *ListenerStore) GetListener(ctx context.Context, exec libdb.Exec, id string) (*Listener, error) {
	row := exec.QueryRowContext(ctx, fmt.Sprintf(`
		SELECT id, %s, kind, target, owner, one_shot, context_filters, metadata, created_at, updated_at
		FROM %s WHERE %s = $1 AND id = $2`,
		s.cfg.ScopeColumn, s.cfg.table("listeners"), s.cfg.ScopeColumn), s.scope, id)
	l, err := scanListener(row)
	if err != nil {
		return nil, err
	}
	l.Types, err = s.listenerTypes(ctx, exec, id)
	return l, err
}

// DeleteListener removes one listener and its topic rows in the caller's
// transaction — the one-shot consumption path when that transaction is the
// firing's own.
func (s *ListenerStore) DeleteListener(ctx context.Context, exec libdb.Exec, id string) error {
	res, err := exec.ExecContext(ctx, fmt.Sprintf(
		`DELETE FROM %s WHERE %s = $1 AND id = $2`,
		s.cfg.table("listeners"), s.cfg.ScopeColumn), s.scope, id)
	if err != nil {
		return fmt.Errorf("libevents: delete listener %q: %w", id, err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		return libdb.ErrNotFound
	}
	_, err = exec.ExecContext(ctx, fmt.Sprintf(
		`DELETE FROM %s WHERE %s = $1 AND listener_id = $2`,
		s.cfg.table("listener_topics"), s.cfg.ScopeColumn), s.scope, id)
	return err
}

// DeleteListenersByOwner removes every listener owner registered, returning
// the ids removed — the cleanup that keeps a dead instance's waits from
// matching forever.
func (s *ListenerStore) DeleteListenersByOwner(ctx context.Context, exec libdb.Exec, owner string) ([]string, error) {
	rows, err := exec.QueryContext(ctx, fmt.Sprintf(
		`SELECT id FROM %s WHERE %s = $1 AND owner = $2`,
		s.cfg.table("listeners"), s.cfg.ScopeColumn), s.scope, owner)
	if err != nil {
		return nil, fmt.Errorf("libevents: list by owner %q: %w", owner, err)
	}
	ids := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, id := range ids {
		if err := s.DeleteListener(ctx, exec, id); err != nil {
			return nil, err
		}
	}
	return ids, nil
}

// ListListenersByType returns every listener subscribed to eventType, via the
// topic index. The caller applies context filters; this is the fan-out read.
func (s *ListenerStore) ListListenersByType(ctx context.Context, exec libdb.Exec, eventType string) ([]*Listener, error) {
	rows, err := exec.QueryContext(ctx, fmt.Sprintf(`
		SELECT l.id, l.%[1]s, l.kind, l.target, l.owner, l.one_shot, l.context_filters, l.metadata, l.created_at, l.updated_at
		FROM %[2]s t JOIN %[3]s l ON l.%[1]s = t.%[1]s AND l.id = t.listener_id
		WHERE t.%[1]s = $1 AND t.event_type = $2
		ORDER BY l.created_at, l.id`,
		s.cfg.ScopeColumn, s.cfg.table("listener_topics"), s.cfg.table("listeners")),
		s.scope, eventType)
	if err != nil {
		return nil, fmt.Errorf("libevents: list by type %q: %w", eventType, err)
	}
	defer rows.Close()
	out, err := scanListeners(rows)
	if err != nil {
		return nil, err
	}
	for _, l := range out {
		if l.Types, err = s.listenerTypes(ctx, exec, l.ID); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// ListListeners returns a page of the scope's listeners, oldest first,
// keyset-paginated on (created_at, id): pass the previous page's last row to
// continue, zero values for the first page.
func (s *ListenerStore) ListListeners(ctx context.Context, exec libdb.Exec, afterCreatedAt time.Time, afterID string, limit int) ([]*Listener, error) {
	if limit <= 0 {
		limit = DefaultListenerLimit
	}
	if limit > MaxListenerLimit {
		limit = MaxListenerLimit
	}
	var rows *sql.Rows
	var err error
	if afterCreatedAt.IsZero() {
		rows, err = exec.QueryContext(ctx, fmt.Sprintf(`
			SELECT id, %s, kind, target, owner, one_shot, context_filters, metadata, created_at, updated_at
			FROM %s WHERE %s = $1
			ORDER BY created_at, id LIMIT $2`,
			s.cfg.ScopeColumn, s.cfg.table("listeners"), s.cfg.ScopeColumn),
			s.scope, limit)
	} else {
		rows, err = exec.QueryContext(ctx, fmt.Sprintf(`
			SELECT id, %s, kind, target, owner, one_shot, context_filters, metadata, created_at, updated_at
			FROM %s WHERE %s = $1 AND (created_at, id) > ($2, $3)
			ORDER BY created_at, id LIMIT $4`,
			s.cfg.ScopeColumn, s.cfg.table("listeners"), s.cfg.ScopeColumn),
			s.scope, afterCreatedAt.UTC(), afterID, limit)
	}
	if err != nil {
		return nil, fmt.Errorf("libevents: list listeners: %w", err)
	}
	defer rows.Close()
	out, err := scanListeners(rows)
	if err != nil {
		return nil, err
	}
	for _, l := range out {
		if l.Types, err = s.listenerTypes(ctx, exec, l.ID); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func (s *ListenerStore) listenerTypes(ctx context.Context, exec libdb.Exec, id string) ([]string, error) {
	rows, err := exec.QueryContext(ctx, fmt.Sprintf(
		`SELECT event_type FROM %s WHERE %s = $1 AND listener_id = $2 ORDER BY event_type`,
		s.cfg.table("listener_topics"), s.cfg.ScopeColumn), s.scope, id)
	if err != nil {
		return nil, fmt.Errorf("libevents: listener %q types: %w", id, err)
	}
	defer rows.Close()
	types := []string{}
	for rows.Next() {
		var t string
		if err := rows.Scan(&t); err != nil {
			return nil, err
		}
		types = append(types, t)
	}
	return types, rows.Err()
}

func scanListener(row libdb.QueryRower) (*Listener, error) {
	var l Listener
	var filters string
	err := row.Scan(&l.ID, &l.Scope, &l.Kind, &l.Target, &l.Owner, &l.OneShot, &filters, &l.Metadata, &l.CreatedAt, &l.UpdatedAt)
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(filters), &l.ContextFilters); err != nil {
		return nil, fmt.Errorf("libevents: decode context filters for %q: %w", l.ID, err)
	}
	return &l, nil
}

func scanListeners(rows *sql.Rows) ([]*Listener, error) {
	out := []*Listener{}
	for rows.Next() {
		var l Listener
		var filters string
		if err := rows.Scan(&l.ID, &l.Scope, &l.Kind, &l.Target, &l.Owner, &l.OneShot, &filters, &l.Metadata, &l.CreatedAt, &l.UpdatedAt); err != nil {
			return nil, fmt.Errorf("libevents: scan listener: %w", err)
		}
		if err := json.Unmarshal([]byte(filters), &l.ContextFilters); err != nil {
			return nil, fmt.Errorf("libevents: decode context filters for %q: %w", l.ID, err)
		}
		out = append(out, &l)
	}
	return out, rows.Err()
}

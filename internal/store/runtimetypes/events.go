package runtimetypes

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	libdb "github.com/contenox/contenox/internal/libdbexec"
)

// Durable domain-event log, date-partitioned: one table per UTC day
// (event_log_YYYYMMDD), so retention is an O(1) DROP of whole periods
// (PruneEventPartitionsBefore) instead of DELETE+VACUUM on the operator's hot
// database.
//
// schema_sqlite.sql holds only the partition registry (event_partitions) and
// the global NID sequence (event_nid_seq); per-period tables are created at
// runtime by EnsureEventPartitionExists — idempotent DDL, so the store holds
// no partition state of its own and concurrent (even cross-process) callers
// converge on one table per period. NIDs come from the single global sequence,
// so ordering is monotonic ACROSS partitions, which is what makes
// ListEventsSince a usable cursor.
//
// Events are WORKSPACE-scoped: every row carries workspace_id and every read
// filters by it, so one workspace's consumers never see another's events.

var (
	ErrEventTypeRequired = errors.New("event_log: event type is required")
	// ErrInvalidEventParameter rejects a malformed argument (nil event,
	// negative hop, inverted time range, a registry row naming a table that
	// does not match the partition-name grammar).
	ErrInvalidEventParameter = errors.New("event_log: invalid parameter")
	// ErrEventMissingRequiredField rejects an append or query without its
	// workspace — the scoping invariant is load-bearing, never defaulted.
	ErrEventMissingRequiredField = errors.New("event_log: missing required field")
	// ErrEventTooOld / ErrEventTooNew reject event times outside the
	// acceptance window, guarding the partition invariant: an accepted event
	// always lands in the current or adjacent period.
	ErrEventTooOld = errors.New("event_log: event is too old")
	ErrEventTooNew = errors.New("event_log: event is too new")
)

// EventAcceptanceWindow bounds how far an event's Time may deviate from now on
// append — ±10 minutes.
const EventAcceptanceWindow = 10 * time.Minute

// MaxEventListLimit bounds one list/query page.
const MaxEventListLimit = 1000

// eventPartitionPrefix + a YYYYMMDD period names one per-day event table.
const eventPartitionPrefix = "event_log_"

// Event is one stored domain event, CloudEvents-ish: Type names what happened
// (and doubles as the bus subject), Source names the producer, Subject the
// entity concerned. WorkspaceID scopes the event; it is required on append.
// Hop is the dispatch-loop guard: an event appended by a chain a trigger fired
// carries its causing event's hop+1.
type Event struct {
	NID         int64           `json:"nid"`
	WorkspaceID string          `json:"workspace_id"`
	Type        string          `json:"type"`
	Source      string          `json:"source,omitempty"`
	Subject     string          `json:"subject,omitempty"`
	Time        time.Time       `json:"time"`
	Data        json.RawMessage `json:"data"`
	Hop         int             `json:"hop"`
}

// EventPartition is one registered per-day event table (row of event_partitions).
type EventPartition struct {
	Period    string
	TableName string
	CreatedAt time.Time
}

// EventStore is the persistence surface over the partitioned event log. Every
// read is workspace-scoped; AppendEvent reads the workspace from the event
// itself.
type EventStore interface {
	// AppendEvent inserts event into its period's table, assigning the next
	// global NID in the same transaction as the sequence bump. Defaults: zero
	// Time becomes now (UTC), nil Data becomes {}, zero Hop is taken from the
	// context (EventHopFromContext). Type and WorkspaceID are required; Time
	// outside the acceptance window is rejected (ErrEventTooOld/ErrEventTooNew).
	AppendEvent(ctx context.Context, event *Event) error
	// ListEventsSince returns workspaceID's events with nid > afterNID in
	// ascending nid order across every partition — the catch-up primitive.
	// limit is clamped to [1, MaxEventListLimit].
	ListEventsSince(ctx context.Context, workspaceID string, afterNID int64, limit int) ([]Event, error)
	// ListRecentEvents returns workspaceID's events in descending nid order —
	// newest first, an operator activity view. beforeNID == 0 starts from the
	// newest event; a positive beforeNID returns events with nid < beforeNID,
	// so the smallest nid in a page is the cursor for the next (older) page.
	ListRecentEvents(ctx context.Context, workspaceID string, beforeNID int64, limit int) ([]Event, error)
	// GetEventsByType returns workspaceID's events of eventType with time in
	// [from, to], newest first.
	GetEventsByType(ctx context.Context, workspaceID, eventType string, from, to time.Time, limit int) ([]Event, error)
	// GetEventsBySource is GetEventsByType narrowed to one producer.
	GetEventsBySource(ctx context.Context, workspaceID, eventType string, from, to time.Time, eventSource string, limit int) ([]Event, error)
	// GetEventsBySubject returns one entity's history in a time range, newest
	// first — the CloudEvents subject standing in for bob2's
	// (aggregate_type, aggregate_id) pair.
	GetEventsBySubject(ctx context.Context, workspaceID, eventType string, from, to time.Time, subject string, limit int) ([]Event, error)
	// GetEventTypesInRange returns the distinct event types workspaceID
	// logged with time in [from, to], sorted.
	GetEventTypesInRange(ctx context.Context, workspaceID string, from, to time.Time, limit int) ([]string, error)
	// DeleteEventsByTypeInRange deletes workspaceID's events of eventType
	// with time in [from, to]. The surgical path —
	// PruneEventPartitionsBefore remains the O(1) retention mechanism; this
	// exists for targeted removal within live periods.
	DeleteEventsByTypeInRange(ctx context.Context, workspaceID, eventType string, from, to time.Time) error
	// EnsureEventPartitionExists creates ts's period table, its indexes, and
	// its registry row when absent. Plain idempotent DDL, holding no process
	// state: it cannot go stale against a partition another process pruned,
	// and concurrent callers are safe. AppendEvent calls it.
	EnsureEventPartitionExists(ctx context.Context, ts time.Time) error
	// ListEventPartitions returns the registered partitions in period order.
	ListEventPartitions(ctx context.Context) ([]EventPartition, error)
	// PruneEventPartitionsBefore drops every partition whose period is
	// strictly before t's period — whole-table DROPs across ALL workspaces
	// (partitions are shared; retention is a per-database decision), O(1) per
	// period — and returns the dropped periods. t's own period always
	// survives. Cursors and firings are untouched.
	PruneEventPartitionsBefore(ctx context.Context, t time.Time) ([]string, error)
}

// eventStore is pure persistence: no cached partition state, so every method
// is safe to call concurrently and from other processes. Memoizing which
// periods exist belongs to the service layer (eventlog's partitionCache).
//
// It takes a DBManager rather than the Exec the rest of this package's stores
// share: AppendEvent must bump the NID sequence and insert the row in ONE
// transaction, and Exec cannot open one.
type eventStore struct {
	db  libdb.DBManager
	now func() time.Time
}

// EventStoreOption configures an event store at construction.
type EventStoreOption func(*eventStore)

// WithEventClock overrides the store's time source — tests crossing period
// boundaries move "now" instead of writing out-of-window times.
func WithEventClock(now func() time.Time) EventStoreOption {
	return func(s *eventStore) {
		if now != nil {
			s.now = now
		}
	}
}

// NewEventStore creates an event-log store bound to db.
func NewEventStore(db libdb.DBManager, opts ...EventStoreOption) EventStore {
	if db == nil {
		panic("SERVER BUG: runtimetypes.NewEventStore called with nil db")
	}
	s := &eventStore{db: db, now: time.Now}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// eventHopContextKey threads the current dispatch hop through execution
// contexts, the same way request IDs travel (libtracker.ContextKeyRequestID):
// the dispatcher stamps hop+1 on a fired chain's context, and every event that
// chain's tools append inherits it.
type eventHopContextKey struct{}

// WithEventHop returns ctx carrying hop for events appended downstream.
// hop <= 0 returns ctx unchanged.
func WithEventHop(ctx context.Context, hop int) context.Context {
	if hop <= 0 {
		return ctx
	}
	return context.WithValue(ctx, eventHopContextKey{}, hop)
}

// EventHopFromContext returns the hop WithEventHop set, or 0.
func EventHopFromContext(ctx context.Context) int {
	hop, _ := ctx.Value(eventHopContextKey{}).(int)
	return hop
}

// EventPeriodKey derives an event time's partition period — UTC daily
// (YYYYMMDD). Exported because the service layer's partition cache keys on it.
func EventPeriodKey(t time.Time) string {
	return t.UTC().Format("20060102")
}

func eventPartitionTableName(period string) string {
	return eventPartitionPrefix + period
}

// validEventPartitionTableName guards every identifier interpolated into
// DDL/DML: exactly the prefix plus eight digits.
func validEventPartitionTableName(name string) bool {
	digits, ok := strings.CutPrefix(name, eventPartitionPrefix)
	if !ok || len(digits) != 8 {
		return false
	}
	for _, c := range digits {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

func (s *eventStore) EnsureEventPartitionExists(ctx context.Context, ts time.Time) error {
	period := EventPeriodKey(ts)
	table := eventPartitionTableName(period)
	if !validEventPartitionTableName(table) {
		return fmt.Errorf("%w: invalid partition table %q", ErrInvalidEventParameter, table)
	}
	exec := s.db.WithoutTransaction()
	if _, err := exec.ExecContext(ctx, fmt.Sprintf(`
		CREATE TABLE IF NOT EXISTS %s (
			nid          INTEGER PRIMARY KEY,
			workspace_id TEXT NOT NULL DEFAULT '',
			type         TEXT NOT NULL,
			source       TEXT NOT NULL DEFAULT '',
			subject      TEXT NOT NULL DEFAULT '',
			time         TIMESTAMP NOT NULL,
			data         TEXT NOT NULL DEFAULT '{}',
			hop          INT NOT NULL DEFAULT 0
		)
	`, table)); err != nil {
		return fmt.Errorf("event_log: create partition %s: %w", table, err)
	}
	// The two access paths every read uses: (workspace_id, nid) for cursor
	// tailing and the recent-activity view, (workspace_id, type) for the
	// query surface. Same pair bob2 indexed per tenant.
	for _, stmt := range []string{
		fmt.Sprintf(`CREATE INDEX IF NOT EXISTS idx_%s_ws_nid ON %s(workspace_id, nid)`, table, table),
		fmt.Sprintf(`CREATE INDEX IF NOT EXISTS idx_%s_ws_type ON %s(workspace_id, type)`, table, table),
	} {
		if _, err := exec.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("event_log: index partition %s: %w", table, err)
		}
	}
	if _, err := exec.ExecContext(ctx, `
		INSERT OR IGNORE INTO event_partitions (period, table_name, created_at)
		VALUES ($1, $2, $3)
	`, period, table, s.now().UTC()); err != nil {
		return fmt.Errorf("event_log: register partition %s: %w", table, err)
	}
	return nil
}

func (s *eventStore) AppendEvent(ctx context.Context, event *Event) error {
	if event == nil {
		return fmt.Errorf("%w: event cannot be nil", ErrInvalidEventParameter)
	}
	event.Type = strings.TrimSpace(event.Type)
	if event.Type == "" {
		return ErrEventTypeRequired
	}
	if strings.TrimSpace(event.WorkspaceID) == "" {
		return fmt.Errorf("%w: workspace_id is required", ErrEventMissingRequiredField)
	}
	now := s.now().UTC()
	if event.Time.IsZero() {
		event.Time = now
	}
	if event.Time.Before(now.Add(-EventAcceptanceWindow)) {
		return ErrEventTooOld
	}
	if event.Time.After(now.Add(EventAcceptanceWindow)) {
		return ErrEventTooNew
	}
	if event.Data == nil {
		event.Data = json.RawMessage(`{}`)
	}
	if event.Hop == 0 {
		event.Hop = EventHopFromContext(ctx)
	}
	if event.Hop < 0 {
		return fmt.Errorf("%w: hop must be non-negative", ErrInvalidEventParameter)
	}
	// Self-ensure keeps store-direct appenders (eventlog's DualPublisher)
	// correct with no cached state: a period dropped since is recreated here,
	// never appended into as if it still existed.
	if err := s.EnsureEventPartitionExists(ctx, event.Time); err != nil {
		return err
	}
	table := eventPartitionTableName(EventPeriodKey(event.Time))

	// Sequence bump and row insert share one transaction, so concurrent
	// appenders (including other processes) never mint a duplicate NID.
	exec, commit, release, err := s.db.WithTransaction(ctx)
	if err != nil {
		return fmt.Errorf("event_log: append tx: %w", err)
	}
	defer func() { _ = release() }()
	if _, err := exec.ExecContext(ctx, `UPDATE event_nid_seq SET last_nid = last_nid + 1 WHERE id = 1`); err != nil {
		return fmt.Errorf("event_log: bump nid: %w", err)
	}
	var nid int64
	if err := exec.QueryRowContext(ctx, `SELECT last_nid FROM event_nid_seq WHERE id = 1`).Scan(&nid); err != nil {
		return fmt.Errorf("event_log: read nid: %w", err)
	}
	if _, err := exec.ExecContext(ctx, fmt.Sprintf(`
		INSERT INTO %s (nid, workspace_id, type, source, subject, time, data, hop)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
	`, table), nid, event.WorkspaceID, event.Type, event.Source, event.Subject, event.Time, string(event.Data), event.Hop); err != nil {
		return fmt.Errorf("event_log: append: %w", err)
	}
	if err := commit(ctx); err != nil {
		return fmt.Errorf("event_log: append commit: %w", err)
	}
	event.NID = nid
	return nil
}

func (s *eventStore) ListEventPartitions(ctx context.Context) ([]EventPartition, error) {
	rows, err := s.db.WithoutTransaction().QueryContext(ctx, `
		SELECT period, table_name, created_at FROM event_partitions ORDER BY period ASC
	`)
	if err != nil {
		return nil, fmt.Errorf("event_partitions: list: %w", err)
	}
	defer rows.Close()
	parts := []EventPartition{}
	for rows.Next() {
		var p EventPartition
		if err := rows.Scan(&p.Period, &p.TableName, &p.CreatedAt); err != nil {
			return nil, fmt.Errorf("event_partitions: scan row: %w", err)
		}
		parts = append(parts, p)
	}
	return parts, rows.Err()
}

// eventPartitionsInRange returns the registered partitions whose period falls
// in [from, to] (inclusive, by day), in period order.
func (s *eventStore) eventPartitionsInRange(ctx context.Context, from, to time.Time) ([]EventPartition, error) {
	parts, err := s.ListEventPartitions(ctx)
	if err != nil {
		return nil, err
	}
	fromKey, toKey := EventPeriodKey(from), EventPeriodKey(to)
	inRange := []EventPartition{}
	for _, p := range parts {
		if p.Period >= fromKey && p.Period <= toKey {
			inRange = append(inRange, p)
		}
	}
	return inRange, nil
}

// eventColumns is the single projection every event read binds, in scan order
// — spelled once so a new column cannot be added to one query and forgotten in
// another (same idiom as hitlApprovalColumns).
const eventColumns = "nid, workspace_id, type, source, subject, time, data, hop"

// ListEventsSince gathers up to limit rows past afterNID from every partition
// that can still hold them (a MAX(nid) probe prunes cold periods), then merges
// by NID. The merge, not period order, upholds the ascending-NID contract: an
// event timed just inside the previous period may carry a higher NID than the
// next period's first row.
func (s *eventStore) ListEventsSince(ctx context.Context, workspaceID string, afterNID int64, limit int) ([]Event, error) {
	if err := requireEventWorkspace(workspaceID); err != nil {
		return nil, err
	}
	if limit <= 0 || limit > MaxEventListLimit {
		limit = MaxEventListLimit
	}
	parts, err := s.ListEventPartitions(ctx)
	if err != nil {
		return nil, err
	}
	exec := s.db.WithoutTransaction()
	events := []Event{}
	for _, p := range parts {
		if !validEventPartitionTableName(p.TableName) {
			return nil, fmt.Errorf("%w: registry names invalid table %q", ErrInvalidEventParameter, p.TableName)
		}
		var maxNID *int64
		if err := exec.QueryRowContext(ctx,
			fmt.Sprintf(`SELECT MAX(nid) FROM %s`, p.TableName)).Scan(&maxNID); err != nil {
			return nil, fmt.Errorf("event_log: max nid of %s: %w", p.TableName, err)
		}
		if maxNID == nil || *maxNID <= afterNID {
			continue
		}
		rows, err := exec.QueryContext(ctx, fmt.Sprintf(`
			SELECT %s FROM %s
			WHERE workspace_id = $1 AND nid > $2
			ORDER BY nid ASC
			LIMIT $3
		`, eventColumns, p.TableName), workspaceID, afterNID, limit)
		if err != nil {
			return nil, fmt.Errorf("event_log: list since %d in %s: %w", afterNID, p.TableName, err)
		}
		batch, err := scanEventRows(rows)
		if err != nil {
			return nil, err
		}
		events = append(events, batch...)
	}
	sort.Slice(events, func(i, j int) bool { return events[i].NID < events[j].NID })
	if len(events) > limit {
		events = events[:limit]
	}
	return events, nil
}

// ListRecentEvents is the descending-nid sibling of ListEventsSince: newest
// first, for an operator activity view.
func (s *eventStore) ListRecentEvents(ctx context.Context, workspaceID string, beforeNID int64, limit int) ([]Event, error) {
	if err := requireEventWorkspace(workspaceID); err != nil {
		return nil, err
	}
	if limit <= 0 || limit > MaxEventListLimit {
		limit = MaxEventListLimit
	}
	parts, err := s.ListEventPartitions(ctx)
	if err != nil {
		return nil, err
	}
	exec := s.db.WithoutTransaction()
	events := []Event{}
	for _, p := range parts {
		if !validEventPartitionTableName(p.TableName) {
			return nil, fmt.Errorf("%w: registry names invalid table %q", ErrInvalidEventParameter, p.TableName)
		}
		rows, err := exec.QueryContext(ctx, fmt.Sprintf(`
			SELECT %s FROM %s
			WHERE workspace_id = $1 AND ($2 = 0 OR nid < $2)
			ORDER BY nid DESC
			LIMIT $3
		`, eventColumns, p.TableName), workspaceID, beforeNID, limit)
		if err != nil {
			return nil, fmt.Errorf("event_log: list recent in %s: %w", p.TableName, err)
		}
		batch, err := scanEventRows(rows)
		if err != nil {
			return nil, err
		}
		events = append(events, batch...)
	}
	sort.Slice(events, func(i, j int) bool { return events[i].NID > events[j].NID })
	if len(events) > limit {
		events = events[:limit]
	}
	return events, nil
}

// queryEventRange runs one filtered SELECT per partition in [from, to] and
// merges newest-first (time DESC, nid DESC tiebreak) — bob2's query ordering
// over the partition set.
func (s *eventStore) queryEventRange(ctx context.Context, from, to time.Time, limit int, where string, args ...any) ([]Event, error) {
	if limit <= 0 || limit > MaxEventListLimit {
		limit = MaxEventListLimit
	}
	parts, err := s.eventPartitionsInRange(ctx, from, to)
	if err != nil {
		return nil, err
	}
	exec := s.db.WithoutTransaction()
	events := []Event{}
	for _, p := range parts {
		if !validEventPartitionTableName(p.TableName) {
			return nil, fmt.Errorf("%w: registry names invalid table %q", ErrInvalidEventParameter, p.TableName)
		}
		query := fmt.Sprintf(`SELECT %s FROM %s WHERE %s ORDER BY nid DESC LIMIT %d`,
			eventColumns, p.TableName, where, limit)
		rows, err := exec.QueryContext(ctx, query, args...)
		if err != nil {
			return nil, fmt.Errorf("event_log: query %s: %w", p.TableName, err)
		}
		batch, err := scanEventRows(rows)
		if err != nil {
			return nil, err
		}
		events = append(events, batch...)
	}
	sort.Slice(events, func(i, j int) bool {
		if events[i].Time.Equal(events[j].Time) {
			return events[i].NID > events[j].NID
		}
		return events[i].Time.After(events[j].Time)
	})
	if len(events) > limit {
		events = events[:limit]
	}
	return events, nil
}

func (s *eventStore) GetEventsByType(ctx context.Context, workspaceID, eventType string, from, to time.Time, limit int) ([]Event, error) {
	if err := requireEventWorkspace(workspaceID); err != nil {
		return nil, err
	}
	return s.queryEventRange(ctx, from, to, limit,
		`workspace_id = $1 AND type = $2 AND time BETWEEN $3 AND $4`,
		workspaceID, eventType, from, to)
}

func (s *eventStore) GetEventsBySource(ctx context.Context, workspaceID, eventType string, from, to time.Time, eventSource string, limit int) ([]Event, error) {
	if err := requireEventWorkspace(workspaceID); err != nil {
		return nil, err
	}
	return s.queryEventRange(ctx, from, to, limit,
		`workspace_id = $1 AND type = $2 AND source = $3 AND time BETWEEN $4 AND $5`,
		workspaceID, eventType, eventSource, from, to)
}

func (s *eventStore) GetEventsBySubject(ctx context.Context, workspaceID, eventType string, from, to time.Time, subject string, limit int) ([]Event, error) {
	if err := requireEventWorkspace(workspaceID); err != nil {
		return nil, err
	}
	return s.queryEventRange(ctx, from, to, limit,
		`workspace_id = $1 AND type = $2 AND subject = $3 AND time BETWEEN $4 AND $5`,
		workspaceID, eventType, subject, from, to)
}

func (s *eventStore) GetEventTypesInRange(ctx context.Context, workspaceID string, from, to time.Time, limit int) ([]string, error) {
	if err := requireEventWorkspace(workspaceID); err != nil {
		return nil, err
	}
	if limit <= 0 || limit > MaxEventListLimit {
		limit = MaxEventListLimit
	}
	parts, err := s.eventPartitionsInRange(ctx, from, to)
	if err != nil {
		return nil, err
	}
	exec := s.db.WithoutTransaction()
	seen := map[string]bool{}
	for _, p := range parts {
		if !validEventPartitionTableName(p.TableName) {
			return nil, fmt.Errorf("%w: registry names invalid table %q", ErrInvalidEventParameter, p.TableName)
		}
		rows, err := exec.QueryContext(ctx, fmt.Sprintf(`
			SELECT DISTINCT type FROM %s
			WHERE workspace_id = $1 AND time BETWEEN $2 AND $3
		`, p.TableName), workspaceID, from, to)
		if err != nil {
			return nil, fmt.Errorf("event_log: types in %s: %w", p.TableName, err)
		}
		for rows.Next() {
			var et string
			if err := rows.Scan(&et); err != nil {
				_ = rows.Close()
				return nil, fmt.Errorf("event_log: scan type: %w", err)
			}
			seen[et] = true
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return nil, err
		}
		_ = rows.Close()
	}
	types := make([]string, 0, len(seen))
	for et := range seen {
		types = append(types, et)
	}
	sort.Strings(types)
	if len(types) > limit {
		types = types[:limit]
	}
	return types, nil
}

func (s *eventStore) DeleteEventsByTypeInRange(ctx context.Context, workspaceID, eventType string, from, to time.Time) error {
	if err := requireEventWorkspace(workspaceID); err != nil {
		return err
	}
	if from.After(to) {
		return fmt.Errorf("%w: invalid time range: from %v is after to %v", ErrInvalidEventParameter, from, to)
	}
	parts, err := s.eventPartitionsInRange(ctx, from, to)
	if err != nil {
		return err
	}
	exec := s.db.WithoutTransaction()
	for _, p := range parts {
		if !validEventPartitionTableName(p.TableName) {
			return fmt.Errorf("%w: registry names invalid table %q", ErrInvalidEventParameter, p.TableName)
		}
		if _, err := exec.ExecContext(ctx, fmt.Sprintf(`
			DELETE FROM %s
			WHERE workspace_id = $1 AND type = $2 AND time BETWEEN $3 AND $4
		`, p.TableName), workspaceID, eventType, from, to); err != nil {
			return fmt.Errorf("event_log: delete %q in %s: %w", eventType, p.TableName, err)
		}
	}
	return nil
}

func (s *eventStore) PruneEventPartitionsBefore(ctx context.Context, t time.Time) ([]string, error) {
	cutoff := EventPeriodKey(t)
	parts, err := s.ListEventPartitions(ctx)
	if err != nil {
		return nil, err
	}
	dropped := []string{}
	for _, p := range parts {
		if p.Period >= cutoff {
			continue
		}
		if !validEventPartitionTableName(p.TableName) {
			return dropped, fmt.Errorf("%w: registry names invalid table %q", ErrInvalidEventParameter, p.TableName)
		}
		// Registry delete and table drop commit together: no window where a
		// registered period points at a missing table.
		exec, commit, release, err := s.db.WithTransaction(ctx)
		if err != nil {
			return dropped, fmt.Errorf("event_log: prune tx: %w", err)
		}
		if _, err := exec.ExecContext(ctx, `DELETE FROM event_partitions WHERE period = $1`, p.Period); err != nil {
			_ = release()
			return dropped, fmt.Errorf("event_log: deregister partition %s: %w", p.Period, err)
		}
		if _, err := exec.ExecContext(ctx, fmt.Sprintf(`DROP TABLE IF EXISTS %s`, p.TableName)); err != nil {
			_ = release()
			return dropped, fmt.Errorf("event_log: drop partition %s: %w", p.TableName, err)
		}
		if err := commit(ctx); err != nil {
			_ = release()
			return dropped, fmt.Errorf("event_log: prune commit %s: %w", p.Period, err)
		}
		_ = release()
		dropped = append(dropped, p.Period)
	}
	return dropped, nil
}

func requireEventWorkspace(workspaceID string) error {
	if strings.TrimSpace(workspaceID) == "" {
		return fmt.Errorf("%w: workspace_id is required", ErrEventMissingRequiredField)
	}
	return nil
}

func scanEventRows(rows *sql.Rows) ([]Event, error) {
	defer rows.Close()
	events := []Event{}
	for rows.Next() {
		var e Event
		// data scans via string, not json.RawMessage, for SQLite TEXT
		// compatibility (see kv.go's getKVScoped).
		var data string
		if err := rows.Scan(&e.NID, &e.WorkspaceID, &e.Type, &e.Source, &e.Subject, &e.Time, &data, &e.Hop); err != nil {
			return nil, fmt.Errorf("event_log: scan row: %w", err)
		}
		e.Data = json.RawMessage(data)
		events = append(events, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("event_log: rows error: %w", err)
	}
	return events, nil
}

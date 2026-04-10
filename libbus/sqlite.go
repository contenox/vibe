package libbus

import (
	"context"
	"database/sql"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
)

// sqlExec is the minimal database interface required by SQLiteBus.
// It is satisfied by libdbexec.Exec (returned by DBManager.WithoutTransaction())
// without requiring any changes to libdbexec.
type sqlExec interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// SQLiteBus implements Messenger over a SQLite database.
//
// Schema tables (bus_events, bus_requests, bus_replies) must exist before use.
// They are part of runtimetypes.SchemaSQLite and are created automatically
// when the CLI database is opened.
//
// Usage:
//
//	bus := libbus.NewSQLite(dbManager.WithoutTransaction())
//	defer bus.Close()
type SQLiteBus struct {
	db     sqlExec
	mu     sync.Mutex
	closed bool
	cancel context.CancelFunc
	wg     sync.WaitGroup

	// poll intervals (tunable for tests)
	eventPoll   time.Duration
	requestPoll time.Duration
}

const (
	defaultEventPoll   = 200 * time.Millisecond
	defaultRequestPoll = 100 * time.Millisecond
	defaultTimeout     = 10 * time.Second
)

// SQLiteBusOptions overrides poll intervals (e.g. tests use 1ms so request/reply is deterministic).
type SQLiteBusOptions struct {
	EventPoll   time.Duration
	RequestPoll time.Duration
}

// NewSQLite creates a SQLite-backed Messenger.
// exec must be the result of dbManager.WithoutTransaction() — it satisfies sqlExec.
func NewSQLite(exec sqlExec) *SQLiteBus {
	return NewSQLiteWithOptions(exec, SQLiteBusOptions{})
}

// NewSQLiteWithOptions is like NewSQLite but allows tuning poll intervals for tests.
func NewSQLiteWithOptions(exec sqlExec, opt SQLiteBusOptions) *SQLiteBus {
	ctx, cancel := context.WithCancel(context.Background())
	ep := opt.EventPoll
	if ep == 0 {
		ep = defaultEventPoll
	}
	rp := opt.RequestPoll
	if rp == 0 {
		rp = defaultRequestPoll
	}
	b := &SQLiteBus{
		db:          exec,
		cancel:      cancel,
		eventPoll:   ep,
		requestPoll: rp,
	}
	// Background cleanup: remove stale events and orphaned requests older than 5 minutes.
	b.wg.Add(1)
	go func() {
		defer b.wg.Done()
		b.runCleanup(ctx)
	}()
	return b
}

// Publish inserts a row into bus_events so Stream subscribers can pick it up.
func (b *SQLiteBus) Publish(ctx context.Context, subject string, data []byte) error {
	b.mu.Lock()
	closed := b.closed
	b.mu.Unlock()
	if closed {
		return ErrConnectionClosed
	}
	_, err := b.db.ExecContext(ctx,
		`INSERT INTO bus_events (subject, data) VALUES (?, ?)`,
		subject, data,
	)
	if err != nil {
		return fmt.Errorf("%w: sqlite publish: %w", ErrMessagePublish, err)
	}
	return nil
}

// Stream starts a polling goroutine that delivers new bus_events for subject to ch.
// The subscription goroutine stops when ctx is cancelled.
func (b *SQLiteBus) Stream(ctx context.Context, subject string, ch chan<- []byte) (Subscription, error) {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return nil, ErrConnectionClosed
	}
	b.mu.Unlock()

	// Snapshot max(id) before returning so a caller Publish cannot be counted as
	// "historical" (race: Publish before this query ran inside the goroutine used to
	// set cursor == new row id and skip the event forever).
	var cursor int64 = -1
	rows, err := b.db.QueryContext(ctx,
		`SELECT COALESCE(MAX(id), 0) FROM bus_events WHERE subject = ?`, subject)
	if err == nil {
		if rows.Next() {
			_ = rows.Scan(&cursor)
		}
		_ = rows.Close()
	}

	subCtx, subCancel := context.WithCancel(ctx)
	sub := &sqliteSubscription{cancel: subCancel}

	b.wg.Add(1)
	go func() {
		defer b.wg.Done()
		defer subCancel()

		ticker := time.NewTicker(b.eventPoll)
		defer ticker.Stop()
		for {
			select {
			case <-subCtx.Done():
				return
			case <-ticker.C:
				rows, err := b.db.QueryContext(subCtx,
					`SELECT id, data FROM bus_events WHERE subject = ? AND id > ? ORDER BY id`,
					subject, cursor,
				)
				if err != nil {
					continue
				}
				for rows.Next() {
					var id int64
					var payload []byte
					if err := rows.Scan(&id, &payload); err != nil {
						continue
					}
					cursor = id
					select {
					case ch <- payload:
					case <-subCtx.Done():
						_ = rows.Close()
						return
					}
				}
				_ = rows.Close()
			}
		}
	}()

	return sub, nil
}

// Serve registers a handler for subject. A polling goroutine picks up rows from
// bus_requests, calls the handler, and writes the reply to bus_replies.
func (b *SQLiteBus) Serve(ctx context.Context, subject string, handler Handler) (Subscription, error) {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return nil, ErrConnectionClosed
	}
	b.mu.Unlock()

	subCtx, subCancel := context.WithCancel(ctx)
	sub := &sqliteSubscription{cancel: subCancel}

	b.wg.Add(1)
	go func() {
		defer b.wg.Done()
		defer subCancel()

		ticker := time.NewTicker(b.requestPoll)
		defer ticker.Stop()
		for {
			select {
			case <-subCtx.Done():
				return
			case <-ticker.C:
				b.processRequests(subCtx, subject, handler)
			}
		}
	}()

	return sub, nil
}

func (b *SQLiteBus) processRequests(ctx context.Context, subject string, handler Handler) {
	rows, err := b.db.QueryContext(ctx,
		`SELECT id, data FROM bus_requests WHERE subject = ? ORDER BY created_at LIMIT 10`,
		subject,
	)
	if err != nil {
		return
	}
	type req struct {
		id   string
		data []byte
	}
	var reqs []req
	for rows.Next() {
		var r req
		if err := rows.Scan(&r.id, &r.data); err == nil {
			reqs = append(reqs, r)
		}
	}
	_ = rows.Close()

	for _, r := range reqs {
		// Use DELETE as an atomic claim lock. Only the worker that actually
		// deletes the row (RowsAffected == 1) proceeds. If another node/goroutine
		// already claimed it, RowsAffected == 0 and we skip.
		res, err := b.db.ExecContext(ctx, `DELETE FROM bus_requests WHERE id = ?`, r.id)
		if err != nil {
			continue
		}
		if affected, _ := res.RowsAffected(); affected == 0 {
			continue // another worker already claimed this request
		}

		reply, err := handler(ctx, r.data)
		replyData := reply
		if err != nil {
			replyData = fmt.Appendf(nil, `{"error":%q}`, err.Error())
		}
		_, _ = b.db.ExecContext(ctx,
			`INSERT OR REPLACE INTO bus_replies (request_id, data) VALUES (?, ?)`,
			r.id, replyData,
		)
	}
}

// Request inserts a request row and polls for the reply until ctx deadline or 10s timeout.
func (b *SQLiteBus) Request(ctx context.Context, subject string, data []byte) ([]byte, error) {
	b.mu.Lock()
	closed := b.closed
	b.mu.Unlock()
	if closed {
		return nil, ErrConnectionClosed
	}

	id := uuid.New().String()
	if data == nil {
		data = []byte{}
	}
	_, err := b.db.ExecContext(ctx,
		`INSERT INTO bus_requests (id, subject, data) VALUES (?, ?, ?)`,
		id, subject, data,
	)
	if err != nil {
		return nil, fmt.Errorf("sqlite request insert: %w", err)
	}

	// Determine timeout: use ctx deadline if set, otherwise default.
	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Now().Add(defaultTimeout)
	}

	ticker := time.NewTicker(b.requestPoll)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			_, _ = b.db.ExecContext(context.Background(), `DELETE FROM bus_requests WHERE id = ?`, id)
			return nil, ErrRequestTimeout
		case <-ticker.C:
			if time.Now().After(deadline) {
				_, _ = b.db.ExecContext(context.Background(), `DELETE FROM bus_requests WHERE id = ?`, id)
				return nil, ErrRequestTimeout
			}
			rows, err := b.db.QueryContext(ctx,
				`SELECT data FROM bus_replies WHERE request_id = ?`, id)
			if err != nil {
				continue
			}
			var reply []byte
			found := false
			if rows.Next() {
				_ = rows.Scan(&reply)
				found = true
			}
			_ = rows.Close()
			if found {
				_, _ = b.db.ExecContext(context.Background(),
					`DELETE FROM bus_replies WHERE request_id = ?`, id)
				return reply, nil
			}
		}
	}
}

// Close stops all background goroutines. The underlying database is NOT closed
// (it is owned by the caller who provided the sqlExec).
func (b *SQLiteBus) Close() error {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return nil
	}
	b.closed = true
	b.mu.Unlock()
	b.cancel()
	b.wg.Wait()
	return nil
}

func (b *SQLiteBus) runCleanup(ctx context.Context) {
	ticker := time.NewTicker(60 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			cutoff := time.Now().Add(-5 * time.Minute).Unix()
			_, _ = b.db.ExecContext(context.Background(),
				`DELETE FROM bus_events WHERE created_at < ?`, cutoff)
			_, _ = b.db.ExecContext(context.Background(),
				`DELETE FROM bus_replies WHERE created_at < ?`, cutoff)
			_, _ = b.db.ExecContext(context.Background(),
				`DELETE FROM bus_requests WHERE created_at < ?`, cutoff)
		}
	}
}

// ── subscription ──────────────────────────────────────────────────────────

type sqliteSubscription struct {
	cancel context.CancelFunc
}

func (s *sqliteSubscription) Unsubscribe() error {
	s.cancel()
	return nil
}

var _ Messenger = (*SQLiteBus)(nil)

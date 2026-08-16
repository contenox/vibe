package libbus

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
)

type sqlExec interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// SQLiteBus implements Messenger over a SQLite database. The bus_events,
// bus_requests and bus_replies tables must exist before use.
type SQLiteBus struct {
	db     sqlExec
	mu     sync.Mutex
	closed bool
	// ctx bounds every background goroutine this bus owns and is cancelled by
	// Close; subscriptions bind to it too.
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	eventPoll   time.Duration
	requestPoll time.Duration
}

const (
	defaultEventPoll   = 200 * time.Millisecond
	defaultRequestPoll = 100 * time.Millisecond
	defaultTimeout     = 10 * time.Second
	// Caps how long Unsubscribe hands pending events to a consumer that is not reading.
	drainTimeout = time.Second
)

// SQLiteBusOptions overrides poll intervals.
type SQLiteBusOptions struct {
	EventPoll   time.Duration
	RequestPoll time.Duration
}

// NewSQLite creates a SQLite-backed Messenger over the result of
// dbManager.WithoutTransaction().
func NewSQLite(exec sqlExec) *SQLiteBus {
	return NewSQLiteWithOptions(exec, SQLiteBusOptions{})
}

// NewSQLiteWithOptions is like NewSQLite but allows tuning poll intervals.
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
		ctx:         ctx,
		cancel:      cancel,
		eventPoll:   ep,
		requestPoll: rp,
	}
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

// Stream starts a polling goroutine that delivers new bus_events for subject to
// ch. It stops when ctx is cancelled.
func (b *SQLiteBus) Stream(ctx context.Context, subject string, ch chan<- []byte) (Subscription, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return nil, ErrConnectionClosed
	}
	b.mu.Unlock()

	// Snapshot max(id) before returning, so a racing Publish is not skipped as
	// historical.
	var cursor int64
	rows, err := b.db.QueryContext(ctx,
		`SELECT COALESCE(MAX(id), 0) FROM bus_events WHERE subject = ?`, subject)
	if err != nil {
		return nil, fmt.Errorf("%w: sqlite stream cursor: %w", ErrStreamSubscriptionFail, err)
	}
	if rows.Next() {
		err = rows.Scan(&cursor)
	}
	if cerr := rows.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = rows.Err()
	}
	if err != nil {
		return nil, fmt.Errorf("%w: sqlite stream cursor: %w", ErrStreamSubscriptionFail, err)
	}

	subCtx, subCancel := context.WithCancel(ctx)
	b.bindToBusLifetime(subCtx, subCancel)
	sub := &sqliteSubscription{
		cancel: subCancel,
		drain:  make(chan struct{}),
		done:   make(chan struct{}),
	}

	b.wg.Add(1)
	go func() {
		defer b.wg.Done()
		defer subCancel()
		defer close(sub.done)

		drainOnce := func(qCtx context.Context) bool {
			rows, err := b.db.QueryContext(qCtx,
				`SELECT id, data FROM bus_events WHERE subject = ? AND id > ? ORDER BY id`,
				subject, cursor,
			)
			if err != nil {
				return false
			}
			defer rows.Close()
			for rows.Next() {
				var id int64
				var payload []byte
				if err := rows.Scan(&id, &payload); err != nil {
					continue
				}
				select {
				case ch <- payload:
					// Advance only on a successful hand-off.
					cursor = id
				case <-qCtx.Done():
					return false
				}
			}
			return true
		}

		// finalDrain delivers events published before Unsubscribe, bounded.
		finalDrain := func() {
			dCtx, dCancel := context.WithTimeout(context.Background(), drainTimeout)
			defer dCancel()
			drainOnce(dCtx)
		}

		ticker := time.NewTicker(b.eventPoll)
		defer ticker.Stop()
		for {
			select {
			case <-subCtx.Done():
				// Unsubscribe cancels subCtx to interrupt a blocked send; honour
				// a pending drain request before leaving.
				select {
				case <-sub.drain:
					finalDrain()
				default:
				}
				return
			case <-sub.drain:
				finalDrain()
				return
			case <-ticker.C:
				drainOnce(subCtx)
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
	b.bindToBusLifetime(subCtx, subCancel)
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

// bindToBusLifetime cancels a subscription context when the bus is closed, even
// if the caller's context never ends.
func (b *SQLiteBus) bindToBusLifetime(subCtx context.Context, subCancel context.CancelFunc) {
	go func() {
		select {
		case <-b.ctx.Done():
			subCancel()
		case <-subCtx.Done():
		}
	}()
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
		// DELETE is the atomic claim lock: only the worker that removed the row
		// proceeds.
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
			// Same wire shape as the NATS and in-memory backends.
			replyData = fmt.Appendf(nil, "error: %s", err.Error())
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
	if err := ctx.Err(); err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return nil, fmt.Errorf("%w: %v", ErrRequestTimeout, err)
		}
		return nil, err
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
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return nil, fmt.Errorf("%w: %v", ErrRequestTimeout, err)
		}
		return nil, fmt.Errorf("sqlite request insert: %w", err)
	}

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
			if errors.Is(ctx.Err(), context.Canceled) {
				return nil, ctx.Err()
			}
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

// Close stops all background goroutines. The underlying database is not closed.
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

type sqliteSubscription struct {
	cancel  context.CancelFunc
	drain   chan struct{}
	done    chan struct{}
	closeMu sync.Mutex
	drained bool
}

func (s *sqliteSubscription) Unsubscribe() error {
	if s.drain == nil || s.done == nil {
		s.cancel()
		return nil
	}
	s.closeMu.Lock()
	if s.drained {
		s.closeMu.Unlock()
		return nil
	}
	s.drained = true
	close(s.drain)
	s.closeMu.Unlock()
	// The subscriber goroutine may be parked on a send nobody is reading.
	s.cancel()
	<-s.done
	return nil
}

var _ Messenger = (*SQLiteBus)(nil)

package libbus_test

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	libbus "github.com/contenox/contenox/libbus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ── helpers ────────────────────────────────────────────────────────────────

const schema = `
CREATE TABLE IF NOT EXISTS bus_events (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    subject    TEXT    NOT NULL,
    data       BLOB    NOT NULL,
    created_at INTEGER NOT NULL DEFAULT (unixepoch('now'))
);
CREATE TABLE IF NOT EXISTS bus_requests (
    id         TEXT    PRIMARY KEY,
    subject    TEXT    NOT NULL,
    data       BLOB    NOT NULL,
    created_at INTEGER NOT NULL DEFAULT (unixepoch('now'))
);
CREATE TABLE IF NOT EXISTS bus_replies (
    request_id TEXT    PRIMARY KEY,
    data       BLOB    NOT NULL,
    created_at INTEGER NOT NULL DEFAULT (unixepoch('now'))
);
`

func newTestDB(t *testing.T) *sql.DB {
	t.Helper()
	// Use a temp-file SQLite so all connections in the *sql.DB pool see the same data.
	// :memory: databases are per-connection and cause cross-goroutine isolation issues.
	dbPath := filepath.Join(t.TempDir(), "bus_test.db")
	// Match libdbexec.NewSQLiteDBManager: WAL + busy_timeout so concurrent
	// Request/Serve goroutines do not fail transient SQLITE_BUSY (same as prod).
	dsn := dbPath
	if !strings.Contains(dsn, "?") {
		dsn += "?"
	} else {
		dsn += "&"
	}
	dsn += "_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)"
	db, err := sql.Open("sqlite", dsn)
	require.NoError(t, err)
	_, err = db.Exec(schema)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func newTestBus(t *testing.T) *libbus.SQLiteBus {
	t.Helper()
	// Faster-than-default polls keep tests fast without relying on wall-clock timeouts.
	b := libbus.NewSQLiteWithOptions(newTestDB(t), libbus.SQLiteBusOptions{
		RequestPoll: 5 * time.Millisecond,
		EventPoll:   5 * time.Millisecond,
	})
	t.Cleanup(func() { _ = b.Close() })
	return b
}

// ── TestUnit_SQLiteBus_Publish_Stream ─────────────────────────────────────

func TestUnit_SQLiteBus_Publish_Stream(t *testing.T) {
	ctx := t.Context()

	b := newTestBus(t)

	ch := make(chan []byte, 4)
	sub, err := b.Stream(ctx, "test.subject", ch)
	require.NoError(t, err)
	defer sub.Unsubscribe()

	require.NoError(t, b.Publish(ctx, "test.subject", []byte("hello")))
	require.NoError(t, b.Publish(ctx, "test.subject", []byte("world")))

	received := make([]string, 0, 2)
	for len(received) < 2 {
		select {
		case msg := <-ch:
			received = append(received, string(msg))
		case <-ctx.Done():
			t.Fatalf("stopped waiting for messages; got %d/2: %v", len(received), ctx.Err())
		}
	}
	assert.Equal(t, []string{"hello", "world"}, received)
}

func TestUnit_SQLiteBus_Publish_NoSubscriberIsNoError(t *testing.T) {
	ctx := t.Context()
	b := newTestBus(t)
	// Publishing without a subscriber should succeed (fire-and-forget).
	require.NoError(t, b.Publish(ctx, "ghost.subject", []byte("silent")))
}

// ── TestUnit_SQLiteBus_Serve_Request ──────────────────────────────────────

func TestUnit_SQLiteBus_Serve_Request(t *testing.T) {
	ctx := t.Context()

	b := newTestBus(t)

	sub, err := b.Serve(ctx, "echo.service", func(_ context.Context, data []byte) ([]byte, error) {
		return append([]byte("echo:"), data...), nil
	})
	require.NoError(t, err)
	defer sub.Unsubscribe()

	reply, err := b.Request(ctx, "echo.service", []byte("ping"))
	require.NoError(t, err)
	assert.Equal(t, "echo:ping", string(reply))
}

func TestUnit_SQLiteBus_Request_NoHandler_Timeout(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()

	b := newTestBus(t)

	_, err := b.Request(ctx, "nobody.home", []byte("hey"))
	require.ErrorIs(t, err, libbus.ErrRequestTimeout)
}

func TestUnit_SQLiteBus_Serve_ErrorReply(t *testing.T) {
	ctx := t.Context()

	b := newTestBus(t)

	sub, err := b.Serve(ctx, "fail.service", func(_ context.Context, _ []byte) ([]byte, error) {
		return nil, errors.New("something went wrong")
	})
	require.NoError(t, err)
	defer sub.Unsubscribe()

	// Request still returns data (the error is serialised in the reply body).
	reply, err := b.Request(ctx, "fail.service", []byte("boom"))
	require.NoError(t, err) // bus itself doesn't fail — error is in the payload
	assert.Contains(t, string(reply), "something went wrong")
}

// ── TestUnit_SQLiteBus_MultipleRequests ───────────────────────────────────

func TestUnit_SQLiteBus_MultipleSequentialRequests(t *testing.T) {
	ctx := t.Context()

	b := newTestBus(t)

	counter := 0
	sub, err := b.Serve(ctx, "counter.service", func(_ context.Context, _ []byte) ([]byte, error) {
		counter++
		return []byte{byte(counter)}, nil
	})
	require.NoError(t, err)
	defer sub.Unsubscribe()

	for i := 1; i <= 5; i++ {
		reply, err := b.Request(ctx, "counter.service", nil)
		require.NoError(t, err)
		assert.Equal(t, byte(i), reply[0])
	}
}

// ── TestUnit_SQLiteBus_Close ───────────────────────────────────────────────

func TestUnit_SQLiteBus_Publish_AfterClose_Returns_Error(t *testing.T) {
	ctx := t.Context()
	b := libbus.NewSQLite(newTestDB(t))

	require.NoError(t, b.Close())
	require.ErrorIs(t, b.Publish(ctx, "any", []byte("x")), libbus.ErrConnectionClosed)
}

func TestUnit_SQLiteBus_Close_Idempotent(t *testing.T) {
	b := libbus.NewSQLite(newTestDB(t))
	require.NoError(t, b.Close())
	require.NoError(t, b.Close()) // second close must not panic or error
}

// ── TestUnit_SQLiteBus_Unsubscribe ────────────────────────────────────────

func TestUnit_SQLiteBus_Stream_UnsubscribeStopsDelivery(t *testing.T) {
	ctx := t.Context()

	b := newTestBus(t)

	ch := make(chan []byte, 8)
	sub, err := b.Stream(ctx, "unsub.subject", ch)
	require.NoError(t, err)

	// Publish + receive one message.
	require.NoError(t, b.Publish(ctx, "unsub.subject", []byte("before")))
	select {
	case msg := <-ch:
		assert.Equal(t, "before", string(msg))
	case <-ctx.Done():
		t.Fatal("did not receive message before unsubscribe")
	}

	// Unsubscribe then publish another.
	require.NoError(t, sub.Unsubscribe())
	require.NoError(t, b.Publish(ctx, "unsub.subject", []byte("after")))

	for range 200 {
		select {
		case msg := <-ch:
			t.Fatalf("received message after Unsubscribe: %q", string(msg))
		default:
		}
		runtime.Gosched()
	}
}

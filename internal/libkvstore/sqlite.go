package libkvstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/contenox/contenox/internal/libdbexec"
)

// Schema is the DDL needed to bootstrap the kv store table in SQLite.
// Call this once after opening the database (NewSQLiteDBManager accepts a schema string).
const SQLiteSchema = `
CREATE TABLE IF NOT EXISTS kv_store (
    key        TEXT    NOT NULL PRIMARY KEY,
    value      TEXT    NOT NULL,
    expires_at INTEGER          -- Unix nanoseconds; NULL means no expiry
);
`

// SQLiteManager implements KVManager on top of a libdbexec.DBManager (SQLite).
type SQLiteManager struct {
	db libdbexec.DBManager
}

// NewSQLiteManager wraps an existing libdbexec.DBManager.
// The caller is responsible for opening the database and applying SQLiteSchema.
func NewSQLiteManager(db libdbexec.DBManager) *SQLiteManager {
	return &SQLiteManager{db: db}
}

// Executor returns a KVExecutor bound to a non-transactional connection. The
// manager is handed along too, so list/set read-modify-write ops can open a
// real transaction; a bare libdbexec.Exec cannot start one.
func (m *SQLiteManager) Executor(_ context.Context) (KVExecutor, error) {
	return &sqliteExecutor{exec: m.db.WithoutTransaction(), db: m.db}, nil
}

// Close closes the underlying database.
func (m *SQLiteManager) Close() error {
	return m.db.Close()
}

// sqliteExecutor implements KVExecutor using a libdbexec.Exec. db is nil for
// executors created internally to run inside an already-open transaction (see
// withWriteTx), which must never try to nest another one.
type sqliteExecutor struct {
	exec libdbexec.Exec
	db   libdbexec.DBManager
}

// ── helpers ──────────────────────────────────────────────────────────────────

func translateSQLiteKVError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, libdbexec.ErrNotFound) {
		return ErrNotFound
	}
	if errors.Is(err, context.Canceled) {
		return context.Canceled
	}
	return fmt.Errorf("libkvstore/sqlite: %w", err)
}

// withWriteTx runs fn inside a transaction that already holds SQLite's write
// lock, so a read-modify-write cycle cannot interleave with another writer's.
// libdbexec only exposes DEFERRED transactions; a deferred tx that reads then
// writes fails to upgrade with SQLITE_BUSY_SNAPSHOT under WAL if another writer
// intervened. Issuing a no-op write first promotes it immediately (BEGIN
// IMMEDIATE semantics) without materialising a row for a missing key. The lock
// is a real database-level lock: it holds across processes sharing the file,
// not just goroutines.
func (e *sqliteExecutor) withWriteTx(ctx context.Context, key Key, fn func(txe *sqliteExecutor) error) error {
	if e.db == nil {
		return fmt.Errorf("libkvstore/sqlite: executor has no transaction capability")
	}
	const maxAttempts = 8 // covers the window where busy_timeout expires under heavy fan-out
	var err error
	for attempt := range maxAttempts {
		err = e.writeTxOnce(ctx, key, fn)
		if err == nil || !isSQLiteBusy(err) || ctx.Err() != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Duration(attempt+1) * time.Millisecond):
		}
	}
	return err
}

func (e *sqliteExecutor) writeTxOnce(ctx context.Context, key Key, fn func(txe *sqliteExecutor) error) error {
	exec, commit, release, err := e.db.WithTransaction(ctx)
	if err != nil {
		return translateSQLiteKVError(err)
	}
	defer release()

	// Acquire the write lock before reading anything (see withWriteTx).
	if _, err := exec.ExecContext(ctx, `UPDATE kv_store SET value = value WHERE key = ?`, key); err != nil {
		return translateSQLiteKVError(err)
	}
	if err := fn(&sqliteExecutor{exec: exec}); err != nil {
		return err
	}
	return translateSQLiteKVError(commit(ctx))
}

// isSQLiteBusy reports whether err is a lock-contention error worth retrying.
// libdbexec has no sentinel for SQLITE_BUSY, so the driver text is all there is.
func isSQLiteBusy(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "SQLITE_BUSY") ||
		strings.Contains(s, "database is locked") ||
		strings.Contains(s, "database table is locked")
}

func expiresAt(ttl time.Duration) *int64 {
	if ttl <= 0 {
		return nil
	}
	ns := time.Now().Add(ttl).UnixNano()
	return &ns
}

// ── KVExecutor: basic operations ─────────────────────────────────────────────

func (e *sqliteExecutor) Get(ctx context.Context, key Key) (json.RawMessage, error) {
	var value string
	var expiresAtNs sql.NullInt64

	err := e.exec.QueryRowContext(ctx,
		`SELECT value, expires_at FROM kv_store WHERE key = ?`, key,
	).Scan(&value, &expiresAtNs)
	if err != nil {
		return nil, translateSQLiteKVError(err)
	}

	// Honour TTL: treat expired entries as not found.
	if expiresAtNs.Valid && time.Now().UnixNano() > expiresAtNs.Int64 {
		_, _ = e.exec.ExecContext(ctx, `DELETE FROM kv_store WHERE key = ?`, key)
		return nil, ErrNotFound
	}

	return json.RawMessage(value), nil
}

func (e *sqliteExecutor) Set(ctx context.Context, key Key, value json.RawMessage) error {
	return e.SetWithTTL(ctx, key, value, 0)
}

func (e *sqliteExecutor) SetWithTTL(ctx context.Context, key Key, value json.RawMessage, ttl time.Duration) error {
	exp := expiresAt(ttl)
	_, err := e.exec.ExecContext(ctx,
		`INSERT INTO kv_store (key, value, expires_at)
		 VALUES (?, ?, ?)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value, expires_at = excluded.expires_at`,
		key, string(value), exp,
	)
	if err != nil {
		return translateSQLiteKVError(err)
	}
	return nil
}

func (e *sqliteExecutor) Delete(ctx context.Context, key Key) error {
	_, err := e.exec.ExecContext(ctx, `DELETE FROM kv_store WHERE key = ?`, key)
	return translateSQLiteKVError(err)
}

func (e *sqliteExecutor) Exists(ctx context.Context, key Key) (bool, error) {
	var count int
	err := e.exec.QueryRowContext(ctx,
		`SELECT COUNT(1) FROM kv_store WHERE key = ? AND (expires_at IS NULL OR expires_at > ?)`,
		key, time.Now().UnixNano(),
	).Scan(&count)
	if err != nil {
		return false, translateSQLiteKVError(err)
	}
	return count > 0, nil
}

func (e *sqliteExecutor) Keys(ctx context.Context, pattern string) ([]Key, error) {
	// ESCAPE is mandatory: SQLite's LIKE has no default escape character, so
	// globToLike's backslash escapes would otherwise match literally.
	likePattern := globToLike(pattern)
	rows, err := e.exec.QueryContext(ctx,
		`SELECT key FROM kv_store WHERE key LIKE ? ESCAPE '\' AND (expires_at IS NULL OR expires_at > ?)`,
		likePattern, time.Now().UnixNano(),
	)
	if err != nil {
		return nil, translateSQLiteKVError(err)
	}
	defer rows.Close()

	var keys []Key
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			return nil, translateSQLiteKVError(err)
		}
		keys = append(keys, k)
	}
	return keys, translateSQLiteKVError(rows.Err())
}

// ── KVExecutor: list operations ───────────────────────────────────────────────
//
// Lists are stored as a JSON array in a single kv_store row.

func (e *sqliteExecutor) listLoad(ctx context.Context, key Key) ([]json.RawMessage, error) {
	raw, err := e.Get(ctx, key)
	if errors.Is(err, ErrNotFound) {
		return []json.RawMessage{}, nil
	}
	if err != nil {
		return nil, err
	}
	var list []json.RawMessage
	if err := json.Unmarshal(raw, &list); err != nil {
		return nil, fmt.Errorf("libkvstore/sqlite list: corrupt data for key %q: %w", key, err)
	}
	return list, nil
}

func (e *sqliteExecutor) listSave(ctx context.Context, key Key, list []json.RawMessage) error {
	data, err := json.Marshal(list)
	if err != nil {
		return fmt.Errorf("libkvstore/sqlite list marshal: %w", err)
	}
	return e.Set(ctx, key, data)
}

// ListPush prepends value (LPUSH semantics), under a write transaction so
// concurrent pushes can't clobber each other, matching valkey's atomic LPUSH.
func (e *sqliteExecutor) ListPush(ctx context.Context, key Key, value json.RawMessage) error {
	return e.withWriteTx(ctx, key, func(txe *sqliteExecutor) error {
		list, err := txe.listLoad(ctx, key)
		if err != nil {
			return err
		}
		list = append([]json.RawMessage{value}, list...) // LPUSH: prepend
		return txe.listSave(ctx, key, list)
	})
}

func (e *sqliteExecutor) ListRange(ctx context.Context, key Key, start, stop int64) ([]json.RawMessage, error) {
	list, err := e.listLoad(ctx, key)
	if err != nil {
		return nil, err
	}
	n := int64(len(list))
	if start < 0 {
		start = max64(0, n+start)
	}
	if stop < 0 {
		stop = n + stop
	} else if stop >= n {
		stop = n - 1
	}
	if start > stop || start >= n {
		return []json.RawMessage{}, nil
	}
	return list[start : stop+1], nil
}

// ListTrim keeps only the given range. Transactional for the reason given on ListPush.
func (e *sqliteExecutor) ListTrim(ctx context.Context, key Key, start, stop int64) error {
	return e.withWriteTx(ctx, key, func(txe *sqliteExecutor) error {
		list, err := txe.listLoad(ctx, key)
		if err != nil {
			return err
		}
		// withWriteTx may retry this closure; the normalisation below isn't idempotent.
		lo, hi := start, stop
		n := int64(len(list))
		if lo < 0 {
			lo = max64(0, n+lo)
		}
		if hi < 0 {
			hi = n + hi
		} else if hi >= n {
			hi = n - 1
		}
		if lo > hi || lo >= n {
			list = []json.RawMessage{}
		} else {
			list = list[lo : hi+1]
		}
		return txe.listSave(ctx, key, list)
	})
}

func (e *sqliteExecutor) ListLength(ctx context.Context, key Key) (int64, error) {
	list, err := e.listLoad(ctx, key)
	if err != nil {
		return 0, err
	}
	return int64(len(list)), nil
}

// ListRPop removes and returns the tail element; transactional so two poppers
// can't return the same element.
func (e *sqliteExecutor) ListRPop(ctx context.Context, key Key) (json.RawMessage, error) {
	var popped json.RawMessage
	err := e.withWriteTx(ctx, key, func(txe *sqliteExecutor) error {
		popped = nil
		list, err := txe.listLoad(ctx, key)
		if err != nil {
			return err
		}
		if len(list) == 0 {
			return ErrNotFound
		}
		popped = list[len(list)-1]
		return txe.listSave(ctx, key, list[:len(list)-1])
	})
	if err != nil {
		return nil, err
	}
	return popped, nil
}

// ── KVExecutor: set operations ────────────────────────────────────────────────
//
// Sets are stored as a JSON array without duplicates in a single kv_store row.

func (e *sqliteExecutor) setLoad(ctx context.Context, key Key) ([]json.RawMessage, error) {
	return e.listLoad(ctx, key) // same storage shape
}

func (e *sqliteExecutor) setSave(ctx context.Context, key Key, members []json.RawMessage) error {
	return e.listSave(ctx, key, members)
}

// SetAdd adds member if absent. Transactional for the reason given on ListPush.
func (e *sqliteExecutor) SetAdd(ctx context.Context, key Key, member json.RawMessage) error {
	return e.withWriteTx(ctx, key, func(txe *sqliteExecutor) error {
		members, err := txe.setLoad(ctx, key)
		if err != nil {
			return err
		}
		for _, m := range members {
			if string(m) == string(member) {
				return nil // already present
			}
		}
		members = append(members, member)
		return txe.setSave(ctx, key, members)
	})
}

func (e *sqliteExecutor) SetMembers(ctx context.Context, key Key) ([]json.RawMessage, error) {
	return e.setLoad(ctx, key)
}

// SetRemove drops member if present. Transactional for the reason given on ListPush.
func (e *sqliteExecutor) SetRemove(ctx context.Context, key Key, member json.RawMessage) error {
	return e.withWriteTx(ctx, key, func(txe *sqliteExecutor) error {
		members, err := txe.setLoad(ctx, key)
		if err != nil {
			return err
		}
		out := members[:0]
		for _, m := range members {
			if string(m) != string(member) {
				out = append(out, m)
			}
		}
		return txe.setSave(ctx, key, out)
	})
}

// ── utilities ─────────────────────────────────────────────────────────────────

// globToLike converts a glob-style pattern (*, ?) to an SQL LIKE pattern (%, _),
// escaping literal %, _, and \. Only correct paired with ESCAPE '\' — see Keys.
func globToLike(pattern string) string {
	out := make([]byte, 0, len(pattern))
	for i := 0; i < len(pattern); i++ {
		switch pattern[i] {
		case '*':
			out = append(out, '%')
		case '?':
			out = append(out, '_')
		case '%', '_', '\\':
			out = append(out, '\\', pattern[i]) // escape native LIKE specials
		default:
			out = append(out, pattern[i])
		}
	}
	return string(out)
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

package libkvstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/contenox/contenox/libdbexec"
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

// Executor returns a KVExecutor bound to a non-transactional connection.
func (m *SQLiteManager) Executor(_ context.Context) (KVExecutor, error) {
	return &sqliteExecutor{exec: m.db.WithoutTransaction()}, nil
}

// Close closes the underlying database.
func (m *SQLiteManager) Close() error {
	return m.db.Close()
}

// sqliteExecutor implements KVExecutor using a libdbexec.Exec.
type sqliteExecutor struct {
	exec libdbexec.Exec
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
		// Lazy delete
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
	// SQLite LIKE uses % and _ wildcards; convert glob-style * to %.
	likePattern := globToLike(pattern)
	rows, err := e.exec.QueryContext(ctx,
		`SELECT key FROM kv_store WHERE key LIKE ? AND (expires_at IS NULL OR expires_at > ?)`,
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

func (e *sqliteExecutor) ListPush(ctx context.Context, key Key, value json.RawMessage) error {
	list, err := e.listLoad(ctx, key)
	if err != nil {
		return err
	}
	list = append([]json.RawMessage{value}, list...) // LPUSH: prepend
	return e.listSave(ctx, key, list)
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

func (e *sqliteExecutor) ListTrim(ctx context.Context, key Key, start, stop int64) error {
	list, err := e.listLoad(ctx, key)
	if err != nil {
		return err
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
		list = []json.RawMessage{}
	} else {
		list = list[start : stop+1]
	}
	return e.listSave(ctx, key, list)
}

func (e *sqliteExecutor) ListLength(ctx context.Context, key Key) (int64, error) {
	list, err := e.listLoad(ctx, key)
	if err != nil {
		return 0, err
	}
	return int64(len(list)), nil
}

func (e *sqliteExecutor) ListRPop(ctx context.Context, key Key) (json.RawMessage, error) {
	list, err := e.listLoad(ctx, key)
	if err != nil {
		return nil, err
	}
	if len(list) == 0 {
		return nil, ErrNotFound
	}
	last := list[len(list)-1]
	if err := e.listSave(ctx, key, list[:len(list)-1]); err != nil {
		return nil, err
	}
	return last, nil
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

func (e *sqliteExecutor) SetAdd(ctx context.Context, key Key, member json.RawMessage) error {
	members, err := e.setLoad(ctx, key)
	if err != nil {
		return err
	}
	for _, m := range members {
		if string(m) == string(member) {
			return nil // already present
		}
	}
	members = append(members, member)
	return e.setSave(ctx, key, members)
}

func (e *sqliteExecutor) SetMembers(ctx context.Context, key Key) ([]json.RawMessage, error) {
	return e.setLoad(ctx, key)
}

func (e *sqliteExecutor) SetRemove(ctx context.Context, key Key, member json.RawMessage) error {
	members, err := e.setLoad(ctx, key)
	if err != nil {
		return err
	}
	out := members[:0]
	for _, m := range members {
		if string(m) != string(member) {
			out = append(out, m)
		}
	}
	return e.setSave(ctx, key, out)
}

// ── utilities ─────────────────────────────────────────────────────────────────

// globToLike converts a Redis/glob-style pattern (using *) to an SQL LIKE pattern (using %).
func globToLike(pattern string) string {
	out := make([]byte, 0, len(pattern))
	for i := 0; i < len(pattern); i++ {
		switch pattern[i] {
		case '*':
			out = append(out, '%')
		case '?':
			out = append(out, '_')
		case '%', '_':
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

package libdbexec

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	_ "modernc.org/sqlite"
)

// sqliteDBManager implements the DBManager interface for SQLite.
// Use for local single-process mode (Contenox Local); the server keeps using Postgres.
type sqliteDBManager struct {
	dbInstance *sql.DB
}

// NewSQLiteDBManager creates a new DBManager for SQLite.
// path is the database file path (e.g. "./.contenox/local.db" or "file:local.db").
// The parent directory is created if missing. schema is applied on open (e.g. runtimetypes.SchemaSQLite).
func NewSQLiteDBManager(ctx context.Context, path string, schema string) (DBManager, error) {
	if err := ensureSQLiteParentDir(path); err != nil {
		return nil, fmt.Errorf("sqlite parent dir: %w", err)
	}
	// Append WAL mode, busy timeout and foreign_keys to the DSN so ALL connections
	// spawned from the pool inherit them.  Previously foreign_keys was set via a
	// single db.ExecContext call which only applied to one leased connection.
	dsn := path
	if !strings.Contains(dsn, "?") {
		dsn += "?"
	} else {
		dsn += "&"
	}
	dsn += "_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("failed to open sqlite database: %w", translateSQLiteError(err))
	}

	if err = db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("sqlite connection failed: %w", translateSQLiteError(err))
	}

	if schema != "" {
		// Execute statements one-by-one so that an ALTER TABLE failing because a
		// column already exists (on an upgraded DB) only skips that one statement —
		// subsequent migrations still run.  The old single-shot ExecContext halted at
		// the first error, leaving any columns added after the failing ALTER missing.
		for _, stmt := range splitSQLStatements(schema) {
			if _, err = db.ExecContext(ctx, stmt); err != nil {
				msg := err.Error()
				if strings.Contains(msg, "duplicate column name") || strings.Contains(msg, "already exists") {
					continue // idempotent: column was added by a previous run
				}
				_ = db.Close()
				return nil, fmt.Errorf("failed to initialize sqlite schema (stmt: %q): %w", stmt, translateSQLiteError(err))
			}
		}
	}

	return &sqliteDBManager{dbInstance: db}, nil
}

// splitSQLStatements splits a SQL script into individual statements, correctly
// handling semicolons inside single-quoted strings, double-quoted identifiers,
// back-tick identifiers, line comments (--) and block comments (/* */).
func splitSQLStatements(script string) []string {
	var stmts []string
	var cur strings.Builder
	inString, inLineComment, inBlockComment := false, false, false
	var quoteChar byte

	for i := 0; i < len(script); i++ {
		c := script[i]

		if inLineComment {
			if c == '\n' {
				inLineComment = false
			}
			cur.WriteByte(c)
			continue
		}
		if inBlockComment {
			if c == '*' && i+1 < len(script) && script[i+1] == '/' {
				inBlockComment = false
				cur.WriteString("*/")
				i++
			} else {
				cur.WriteByte(c)
			}
			continue
		}

		if !inString {
			if c == '-' && i+1 < len(script) && script[i+1] == '-' {
				inLineComment = true
				cur.WriteString("--")
				i++
				continue
			}
			if c == '/' && i+1 < len(script) && script[i+1] == '*' {
				inBlockComment = true
				cur.WriteString("/*")
				i++
				continue
			}
		}

		if c == '\'' || c == '"' || c == '`' {
			if !inString {
				inString = true
				quoteChar = c
			} else if c == quoteChar {
				if i+1 < len(script) && script[i+1] == quoteChar {
					// Escaped quote ('' or "" or ``)
					cur.WriteByte(c)
					cur.WriteByte(script[i+1])
					i++
					continue
				}
				inString = false
			}
		}

		if !inString && c == ';' {
			if stmt := strings.TrimSpace(cur.String()); stmt != "" {
				stmts = append(stmts, stmt)
			}
			cur.Reset()
			continue
		}
		cur.WriteByte(c)
	}
	if stmt := strings.TrimSpace(cur.String()); stmt != "" {
		stmts = append(stmts, stmt)
	}
	return stmts
}

// WithoutTransaction returns an executor that uses the connection pool directly.
func (sm *sqliteDBManager) WithoutTransaction() Exec {
	return &txAwareDB{db: sm.dbInstance, errTranslate: translateSQLiteError, driverName: "sqlite"}
}

// WithTransaction starts a SQLite transaction and returns executor, commit, and release.
func (sm *sqliteDBManager) WithTransaction(ctx context.Context, onRollback ...func()) (Exec, CommitTx, ReleaseTx, error) {
	tx, err := sm.dbInstance.BeginTx(ctx, nil)
	if err != nil {
		return nil, nil, func() error { return nil }, fmt.Errorf("%w: begin transaction failed: %w", ErrTxFailed, translateSQLiteError(err))
	}

	store := &txAwareDB{tx: tx, errTranslate: translateSQLiteError, driverName: "sqlite"}
	// finalized guards against double-execution of onRollback hooks when
	// releaseFn is deferred and commit also failed (both paths ran rollback logic).
	finalized := false
	fireRollback := func() {
		for _, f := range onRollback {
			if f != nil {
				f()
			}
		}
		onRollback = nil // prevent a second call from re-running any hooks
	}

	commitFn := func(commitCtx context.Context) error {
		if ctxErr := commitCtx.Err(); ctxErr != nil {
			return fmt.Errorf("%w: context error before commit: %w", ErrTxFailed, ctxErr)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("%w: commit failed: %w", ErrTxFailed, translateSQLiteError(err))
		}
		finalized = true
		return nil
	}

	releaseFn := func() error {
		rollbackErr := tx.Rollback()
		if !finalized {
			finalized = true
			fireRollback()
		}
		if rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
			return fmt.Errorf("%w: rollback failed: %w", ErrTxFailed, translateSQLiteError(rollbackErr))
		}
		return nil
	}

	return store, commitFn, releaseFn, nil
}

// Close closes the SQLite connection.
func (sm *sqliteDBManager) Close() error {
	if sm.dbInstance != nil {
		return sm.dbInstance.Close()
	}
	return nil
}

// translateSQLiteError maps SQLite/driver errors to package errors where applicable.
// It wraps with %w so callers can still inspect the original error via errors.Is/As.
func translateSQLiteError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: %w", ErrNotFound, err)
	}
	if errors.Is(err, context.Canceled) {
		return fmt.Errorf("%w: %w", ErrQueryCanceled, context.Canceled)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("%w: %w", ErrQueryCanceled, context.DeadlineExceeded)
	}
	s := err.Error()
	// Wrap with %w (not just return sentinel) so the underlying constraint details
	// (table name, column name) are preserved and visible in logs.
	if strings.Contains(s, "UNIQUE constraint") {
		return fmt.Errorf("%w: %w", ErrUniqueViolation, err)
	}
	if strings.Contains(s, "FOREIGN KEY constraint") {
		return fmt.Errorf("%w: %w", ErrForeignKeyViolation, err)
	}
	if strings.Contains(s, "NOT NULL constraint") {
		return fmt.Errorf("%w: %w", ErrNotNullViolation, err)
	}
	if strings.Contains(s, "CHECK constraint") {
		return fmt.Errorf("%w: %w", ErrCheckViolation, err)
	}
	return fmt.Errorf("libdb: sqlite error: %w", err)
}

// ensureSQLiteParentDir creates the parent directory of path if path is a file path.
// Skips for :memory:. Uses the path before any ? query for file: URIs.
func ensureSQLiteParentDir(path string) error {
	if path == "" || path == ":memory:" || strings.HasPrefix(path, "file::memory") {
		return nil
	}
	fsPath := path
	if strings.HasPrefix(fsPath, "file:") {
		fsPath = strings.TrimPrefix(fsPath, "file:")
		if before, _, ok := strings.Cut(fsPath, "?"); ok {
			fsPath = before
		}
	}
	dir := filepath.Dir(fsPath)
	if dir == "" || dir == "." {
		return nil
	}
	return os.MkdirAll(dir, 0755)
}

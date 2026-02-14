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
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("failed to open sqlite database: %w", translateSQLiteError(err))
	}

	if err = db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("sqlite connection failed: %w", translateSQLiteError(err))
	}

	// Enable foreign keys (SQLite does not enforce them by default)
	if _, err = db.ExecContext(ctx, "PRAGMA foreign_keys = ON"); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("sqlite foreign_keys pragma failed: %w", translateSQLiteError(err))
	}

	if schema != "" {
		if _, err = db.ExecContext(ctx, schema); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("failed to initialize sqlite schema: %w", translateSQLiteError(err))
		}
	}

	return &sqliteDBManager{dbInstance: db}, nil
}

// WithoutTransaction returns an executor that uses the connection pool directly.
func (sm *sqliteDBManager) WithoutTransaction() Exec {
	return &txAwareDB{db: sm.dbInstance}
}

// WithTransaction starts a SQLite transaction and returns executor, commit, and release.
func (sm *sqliteDBManager) WithTransaction(ctx context.Context, onRollback ...func()) (Exec, CommitTx, ReleaseTx, error) {
	tx, err := sm.dbInstance.BeginTx(ctx, nil)
	if err != nil {
		return nil, nil, func() error { return nil }, fmt.Errorf("%w: begin transaction failed: %w", ErrTxFailed, translateSQLiteError(err))
	}

	store := &txAwareDB{tx: tx}
	committed := false
	rollback := func() {
		for _, f := range onRollback {
			if f != nil {
				f()
			}
		}
	}

	commitFn := func(commitCtx context.Context) error {
		if ctxErr := commitCtx.Err(); ctxErr != nil {
			return fmt.Errorf("%w: context error before commit: %w", ErrTxFailed, ctxErr)
		}
		err := tx.Commit()
		if err != nil {
			return fmt.Errorf("%w: commit failed: %w", ErrTxFailed, translateSQLiteError(err))
		}
		committed = true
		return nil
	}

	releaseFn := func() error {
		rollbackErr := tx.Rollback()
		if !committed {
			rollback()
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
		// log.Println("Closing SQLite database connection.")
		return sm.dbInstance.Close()
	}
	return nil
}

// translateSQLiteError maps SQLite/driver errors to package errors where applicable.
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
	// SQLite constraint errors: check error string for common codes
	if err != nil {
		s := err.Error()
		if strings.Contains(s, "UNIQUE constraint") {
			return ErrUniqueViolation
		}
		if strings.Contains(s, "FOREIGN KEY constraint") {
			return ErrForeignKeyViolation
		}
		if strings.Contains(s, "NOT NULL constraint") {
			return ErrNotNullViolation
		}
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

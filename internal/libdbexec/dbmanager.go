package libdbexec

import (
	"context"
	"database/sql"
	"errors"
)

// Predefined errors for common database interaction scenarios.
// Using these allows application code to check for specific conditions
// using errors.Is without relying on driver-specific error types or codes.
var (
	// ErrNotFound is returned by Scan when sql.ErrNoRows is encountered.
	ErrNotFound = errors.New("libdb: not found")

	// ErrTxFailed indicates a failure during transaction finalization (Commit or Rollback).
	ErrTxFailed = errors.New("libdb: transaction failed")

	// ErrMaxRowsReached indicates that the maximum number of rows on a given table has been reached.
	// This error should be thrown when attempting to create a new entry would lead to exceeding the maximum capacity.
	// It implies that enforcing a reasonable maximum row count is necessary to prevent operations like bulk update from failing.
	ErrMaxRowsReached = errors.New("max row count reached")

	// --- Constraint Violations ---

	// ErrUniqueViolation corresponds to unique key constraint errors (e.g., PostgreSQL code 23505).
	ErrUniqueViolation = errors.New("libdb: unique constraint violation")
	// ErrForeignKeyViolation corresponds to foreign key constraint errors (e.g., PostgreSQL code 23503).
	ErrForeignKeyViolation = errors.New("libdb: foreign key violation")
	// ErrNotNullViolation corresponds to not-null constraint errors (e.g., PostgreSQL code 23502).
	ErrNotNullViolation = errors.New("libdb: not null constraint violation")
	// ErrCheckViolation corresponds to check constraint errors (e.g., PostgreSQL code 23514).
	ErrCheckViolation = errors.New("libdb: check constraint violation")
	// ErrConstraintViolation is a generic error for constraint violations not specifically mapped.
	ErrConstraintViolation = errors.New("libdb: constraint violation")

	// --- Operational Errors ---

	// ErrDeadlockDetected corresponds to deadlock errors (e.g., PostgreSQL code 40P01).
	ErrDeadlockDetected = errors.New("libdb: deadlock detected")
	// ErrSerializationFailure corresponds to serialization failures (e.g., PostgreSQL code 40001).
	ErrSerializationFailure = errors.New("libdb: serialization failure")
	// ErrLockNotAvailable corresponds to lock acquisition failures (e.g., PostgreSQL code 55P03).
	ErrLockNotAvailable = errors.New("libdb: lock not available")
	// ErrQueryCanceled corresponds to query cancellation (e.g., PostgreSQL code 57014 or context cancellation).
	ErrQueryCanceled = errors.New("libdb: query canceled")

	// --- Data Errors ---

	// ErrDataTruncation corresponds to data truncation errors (e.g., PostgreSQL code 22001).
	ErrDataTruncation = errors.New("libdb: data truncation error")
	// ErrNumericOutOfRange corresponds to numeric overflow errors (e.g., PostgreSQL code 22003).
	ErrNumericOutOfRange = errors.New("libdb: numeric value out of range")
	// ErrInvalidInputSyntax corresponds to syntax errors in data representation (e.g., PostgreSQL code 22P02).
	ErrInvalidInputSyntax = errors.New("libdb: invalid input syntax")

	// --- Schema Errors ---

	// ErrUndefinedColumn corresponds to referencing an unknown column (e.g., PostgreSQL code 42703).
	ErrUndefinedColumn = errors.New("libdb: undefined column")
	// ErrUndefinedTable corresponds to referencing an unknown table (e.g., PostgreSQL code 42P01).
	ErrUndefinedTable = errors.New("libdb: undefined table")
)

// DBManager defines the interface for obtaining database executors and managing
// the database connection lifecycle. It serves as the main entry point for database interactions.
//
// Usage Example (Transaction):
// 	func handleRequest(ctx context.Context, mgr libdb.DBManager) error {
// 	    // Start transaction, get executor and commit/release functions
// 	    exec, commit, release, err := mgr.WithTransaction(ctx)
// 	    if err != nil {
// 	        return fmt.Errorf("failed to start transaction: %w", err)
// 	    }
//
// 	    // Always defer release() to ensure cleanup (rollback on error/panic, no-op after commit)
// 	    defer release()

//	    // --- Do work using exec ---
//	    _, err = exec.ExecContext(ctx, "UPDATE settings SET value = $1 WHERE key = $2", "new_value", "setting_key")
//	    if err != nil {
//	        // Error occurred - no need to call release explicitly, defer handles it.
//	        return fmt.Errorf("failed to update setting: %w", err)
//	    }
//
//	    // --- Success ---
//	    // Attempt to commit; if it fails, the deferred release() still runs.
//	    if err = commit(ctx); err != nil {
//	        return fmt.Errorf("transaction commit failed: %w", err)
//	    }
//
//	    // Commit successful. The deferred release() will run but do nothing (idempotent).
//	    return nil
//	}
type DBManager interface {
	// WithoutTransaction returns an executor that operates directly on the underlying
	// database connection group (i.e., outside of an explicit transaction).
	// Each operation may run on a different connection.
	WithoutTransaction() Exec

	// WithTransaction starts a new database transaction and returns:
	//   - Exec:       An executor bound to the transaction for executing queries
	//   - CommitTx:   Function to commit the transaction (call only on success path)
	//   - ReleaseTx:  Function to release resources (designed for defer, handles rollback)
	//   - error:      Non-nil if transaction couldn't be started
	//
	// Parameters:
	//   ctx:        Context for transaction initialization (timeout/cancellation)
	//   onRollback: Optional functions executed ONLY if transaction is rolled back.
	//               Called AFTER successful rollback. Must NOT use the transaction.
	WithTransaction(ctx context.Context, onRollback ...func()) (Exec, CommitTx, ReleaseTx, error)

	// Close terminates the underlying database connection group.
	// It should be called when the application is shutting down.
	Close() error
}

// Exec defines the common interface for executing database operations,
// whether within a transaction or directly on the connection group.
// Errors returned by methods implementing this interface should be translated
// into the package's predefined Err* variables where applicable.
type Exec interface {
	// ExecContext executes a query without returning any rows.
	// The args are for any placeholder parameters in the query.
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)

	// QueryContext executes a query that returns rows, typically a SELECT.
	// The args are for any placeholder parameters in the query.
	// Callers should check rows.Err() after iterating.
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)

	// QueryRowContext executes a query that is expected to return at most one row.
	// QueryRowContext always returns a non-nil value. Errors are deferred until
	// QueryRower's Scan method is called.
	QueryRowContext(ctx context.Context, query string, args ...any) QueryRower

	// DriverName returns the name of the database driver ("postgres", "sqlite").
	DriverName() string
}

// QueryRower provides the Scan method, typically implemented by wrapping *sql.Row.
// Using this interface allows Scan errors (like sql.ErrNoRows) to be translated
// consistently by the library.
type QueryRower interface {
	// Scan copies the columns from the matched row into the values pointed at by dest.
	// If no rows were found, it returns ErrNotFound. Other scan errors are translated.
	Scan(dest ...any) error
}

// CommitTx is a function type responsible for attempting to commit a transaction.
// It should typically only be called on the success path of a transactional operation.
// It may check the context before committing and returns ErrTxFailed or a translated
// database error if the commit fails.
// Returns:
//   - nil:        Successfully committed
//   - ErrTxFailed: Wrapped error if commit failed (transaction already rolled back)
//   - context error: If ctx is done before commit attempt
type CommitTx func(ctx context.Context) error

// ReleaseTx is a function type responsible for rolling back a transaction, ensuring
// its resources are released. It is designed to be idempotent (safe to call multiple times
// or after a commit) and is ideal for use with `defer` to guarantee cleanup.
// It returns ErrTxFailed or a translated database error if the rollback fails
// (and the transaction wasn't already finalized).
// recap: it safely releases transaction resources:
//   - Rolls back if not committed
//   - No-op if already committed/rolled back
//   - Executes onRollback handlers only after successful rollback
//
// Returns:
//   - nil:        Success or no-op (ErrTxDone)
//   - ErrTxFailed: Wrapped error if rollback unexpectedly failed
type ReleaseTx func() error

// txAwareDB implements the Exec interface, delegating to an underlying
// *sql.DB or *sql.Tx and translating errors via an injected translator.
// This allows each database driver (Postgres, SQLite, etc.) to wire
// in its own error mapping so sentinel errors like ErrUniqueViolation
// are always returned correctly regardless of driver.
type txAwareDB struct {
	db           *sql.DB
	tx           *sql.Tx
	errTranslate func(error) error // driver-specific error translator
	driverName   string
}

// ExecContext delegates to the underlying DB or Tx and translates errors.
func (s *txAwareDB) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	var res sql.Result
	var err error
	if s.tx != nil {
		res, err = s.tx.ExecContext(ctx, query, args...)
	} else if s.db != nil {
		res, err = s.db.ExecContext(ctx, query, args...)
	} else {
		return nil, errors.New("libdb: Exec called on uninitialized txAwareDB")
	}
	return res, s.errTranslate(err)
}

// QueryContext delegates to the underlying DB or Tx and translates errors.
func (s *txAwareDB) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	var rows *sql.Rows
	var err error
	if s.tx != nil {
		rows, err = s.tx.QueryContext(ctx, query, args...)
	} else if s.db != nil {
		rows, err = s.db.QueryContext(ctx, query, args...)
	} else {
		return nil, errors.New("libdb: Query called on uninitialized txAwareDB")
	}
	if err != nil {
		return nil, s.errTranslate(err)
	}
	return rows, nil
}

// QueryRowContext delegates to the underlying DB or Tx and wraps the result.
func (s *txAwareDB) QueryRowContext(ctx context.Context, query string, args ...any) QueryRower {
	var r *sql.Row
	if s.tx != nil {
		r = s.tx.QueryRowContext(ctx, query, args...)
	} else if s.db != nil {
		r = s.db.QueryRowContext(ctx, query, args...)
	} else {
		return &row{err: errors.New("libdb: QueryRow called on uninitialized txAwareDB")}
	}
	return &row{inner: r, errTranslate: s.errTranslate}
}

func (s *txAwareDB) DriverName() string {
	return s.driverName
}

// row implements QueryRower, wrapping *sql.Row to translate Scan errors.
type row struct {
	inner        *sql.Row
	err          error
	errTranslate func(error) error
}

// Scan calls the underlying Scan method and translates the error.
func (r *row) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	if r.inner == nil {
		return errors.New("libdb: Scan called on nil row wrapper")
	}
	return r.errTranslate(r.inner.Scan(dest...))
}

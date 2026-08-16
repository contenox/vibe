package libdbexec

import (
	"context"
	"database/sql"
	"errors"
)

// Predefined errors, checkable with errors.Is.
var (
	// ErrNotFound is returned by Scan when sql.ErrNoRows is encountered.
	ErrNotFound = errors.New("libdb: not found")

	// ErrTxFailed indicates a failure during transaction finalization (Commit or Rollback).
	ErrTxFailed = errors.New("libdb: transaction failed")

	// ErrMaxRowsReached indicates a table's configured maximum row count would be exceeded.
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

// DBManager is the main entry point for database interactions: obtaining
// executors and managing the connection lifecycle.
type DBManager interface {
	// WithoutTransaction returns an executor operating directly on the connection
	// group, outside any transaction; each operation may run on a different connection.
	WithoutTransaction() Exec

	// WithTransaction starts a transaction and returns an Exec bound to it, a
	// CommitTx, and a ReleaseTx (idempotent, safe for defer, rolls back if not
	// committed). onRollback handlers run only after a successful rollback and
	// must not touch the transaction.
	WithTransaction(ctx context.Context, onRollback ...func()) (Exec, CommitTx, ReleaseTx, error)

	// Close terminates the underlying database connection group.
	Close() error
}

// Exec is the common interface for executing database operations, whether
// within a transaction or directly on the connection group. Implementations
// must translate driver errors into the package's Err* sentinels.
type Exec interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)

	// QueryContext executes a query returning rows. Callers must check rows.Err() after iterating.
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)

	// QueryRowContext always returns a non-nil QueryRower; errors surface from its Scan.
	QueryRowContext(ctx context.Context, query string, args ...any) QueryRower

	// DriverName returns the database driver name ("postgres", "sqlite").
	DriverName() string
}

// QueryRower wraps *sql.Row so Scan errors (like sql.ErrNoRows) are translated consistently.
type QueryRower interface {
	// Scan returns ErrNotFound if no row matched; other errors are translated too.
	Scan(dest ...any) error
}

// CommitTx commits a transaction; call only on the success path. Returns nil,
// a wrapped ErrTxFailed, or a context error if ctx is done before the attempt.
type CommitTx func(ctx context.Context) error

// ReleaseTx rolls back a transaction if it wasn't committed and is a no-op
// otherwise; idempotent and meant for defer.
type ReleaseTx func() error

// txAwareDB implements Exec, delegating to *sql.DB or *sql.Tx and translating
// errors via an injected per-driver translator.
type txAwareDB struct {
	db           *sql.DB
	tx           *sql.Tx
	errTranslate func(error) error // driver-specific error translator
	driverName   string
}

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

func (r *row) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	if r.inner == nil {
		return errors.New("libdb: Scan called on nil row wrapper")
	}
	return r.errTranslate(r.inner.Scan(dest...))
}

// Package libdbexec provides driver-agnostic interfaces (DBManager, Exec,
// QueryRower) for SQL access, implemented for PostgreSQL (lib/pq) and SQLite.
// WithTransaction pairs a CommitTx with a ReleaseTx meant for defer, and
// low-level driver errors are translated to package-level sentinels
// (ErrNotFound, ErrUniqueViolation, ErrDeadlockDetected, ...).
package libdbexec

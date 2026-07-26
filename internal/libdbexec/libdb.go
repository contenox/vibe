// Package dbexec provides an interface for interacting with
// a SQL database, currently with a specific implementation for PostgreSQL using lib/pq.

// Key Features:

//  1. Abstraction: Defines interfaces (`DBManager`, `Exec`, `QueryRower`) to decouple
//     application code from specific database driver details.

//  2. Simplified Transaction Management: The `DBManager.WithTransaction` method
//     provides a clear pattern for handling database transactions, returning
//     separate functions for committing (`CommitTx`) and releasing/rolling back
//     (`ReleaseTx`). The `ReleaseTx` function is designed for use with `defer`
//     to ensure transactions are always finalized and connections are released,
//     even in cases of errors or panics.

//  3. Centralized Error Translation: Maps common low-level database errors
//     (like sql.ErrNoRows or PostgreSQL-specific pq.Error codes) to a consistent
//     set of exported package errors (e.g., ErrNotFound, ErrUniqueViolation,
//     ErrDeadlockDetected). This simplifies error handling in application code.
package libdbexec

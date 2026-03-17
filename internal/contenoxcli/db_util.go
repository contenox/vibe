package contenoxcli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	libdb "github.com/contenox/contenox/libdbexec"
	"github.com/contenox/contenox/libkvstore"
	"github.com/contenox/contenox/runtimetypes"
)

// openDBAt opens (and creates if needed) the SQLite database at the given path.
// It applies the application schema and the KV store schema so the kv_store table
// is always present for provider model-list caching.
func openDBAt(ctx context.Context, dbPath string) (libdb.DBManager, error) {
	if err := os.MkdirAll(filepath.Dir(dbPath), 0755); err != nil {
		return nil, fmt.Errorf("cannot create database directory: %w", err)
	}
	schema := runtimetypes.SchemaSQLite + "\n" + libkvstore.SQLiteSchema
	db, err := libdb.NewSQLiteDBManager(ctx, dbPath, schema)
	if err != nil {
		return nil, fmt.Errorf("failed to open database %q: %w", dbPath, err)
	}
	return db, nil
}

// withTransaction is a convenience wrapper around DBManager.WithTransaction.
// It handles the boilerplate (defer release, check commit) so callers only
// supply the work function.
func withTransaction(ctx context.Context, db libdb.DBManager, fn func(tx libdb.Exec) error) error {
	txExec, commit, release, err := db.WithTransaction(ctx)
	if err != nil {
		return fmt.Errorf("failed to start transaction: %w", err)
	}
	defer release()
	if err := fn(txExec); err != nil {
		return err
	}
	if err := commit(ctx); err != nil {
		return fmt.Errorf("failed to commit transaction: %w", err)
	}
	return nil
}

// WithTransaction is the exported version for use by sub-packages.
func WithTransaction(ctx context.Context, db libdb.DBManager, fn func(tx libdb.Exec) error) error {
	return withTransaction(ctx, db, fn)
}

package contenoxcli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/contenox/contenox/internal/store/runtimetypes"
	libdb "github.com/contenox/contenox/libdbexec"
	"github.com/contenox/contenox/libkvstore"
)

// OpenDBAt opens (and creates if needed) the SQLite database at the given path,
// applying the application and KV store schemas.
func OpenDBAt(ctx context.Context, dbPath string) (libdb.DBManager, error) {
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

// WithTransaction wraps DBManager.WithTransaction, handling release and commit.
func WithTransaction(ctx context.Context, db libdb.DBManager, fn func(tx libdb.Exec) error) error {
	return withTransaction(ctx, db, fn)
}

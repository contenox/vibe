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

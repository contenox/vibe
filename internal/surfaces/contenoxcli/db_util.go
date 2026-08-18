package contenoxcli

import (
	"context"
	"fmt"

	"github.com/contenox/contenox/internal/substrate"
	libdb "github.com/contenox/contenox/libdbexec"
)

func OpenDBAt(ctx context.Context, dbPath string) (libdb.DBManager, error) {
	return substrate.OpenDB(ctx, dbPath, dbPathIsExplicit(dbPath))
}

// dbPathIsExplicit reports whether dbPath is something other than the default global path.
func dbPathIsExplicit(dbPath string) bool {
	def, err := globalDBPath()
	if err != nil {
		return false
	}
	return dbPath != def
}

func openOptionalDB(ctx context.Context, dbPath string) (libdb.DBManager, error) {
	db, err := OpenDBAt(ctx, dbPath)
	if err == nil {
		return db, nil
	}
	sel, resolveErr := substrate.Resolve()
	if resolveErr != nil || sel.UsesPostgres() {
		return nil, err
	}
	return nil, nil
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

package contenoxcli

import (
	"context"
	"fmt"

	"github.com/contenox/contenox/internal/substrate"
	libdb "github.com/contenox/contenox/libdbexec"
)

func OpenDBAt(ctx context.Context, dbPath string) (libdb.DBManager, error) {
	return substrate.OpenDB(ctx, dbPath)
}

func openOptionalDB(ctx context.Context, dbPath string) (libdb.DBManager, error) {
	db, err := OpenDBAt(ctx, dbPath)
	if err == nil {
		return db, nil
	}
	if substrate.Configured() {
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

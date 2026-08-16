package contenoxcli

import (
	"context"
	"fmt"

	"github.com/contenox/contenox/internal/models/runtimestate"
	"github.com/contenox/contenox/libkvstore"
	"github.com/contenox/contenox/libtracker"
	"github.com/spf13/cobra"
)

var cacheCmd = &cobra.Command{
	Use:   "cache",
	Short: "Manage data Contenox caches between runs.",
	Long: `Inspect and clear data Contenox caches between runs.

The chat/run engine caches each backend's model list for up to an hour to avoid
re-querying the provider on every invocation. That cache is keyed per backend, so
` + "`contenox model list`" + ` (which always fetches live) can show models the chat
path hasn't picked up yet — e.g. right after upgrading contenox or editing a
backend. Clear the cache to force a fresh fetch.`,
}

var cacheClearCmd = &cobra.Command{
	Use:   "clear",
	Short: "Clear cached backend model lists so the next chat/run refetches.",
	Long: `Delete the cached per-backend model lists (the prov:* keys the chat/run
engine reads). Use this when ` + "`contenox model list`" + ` shows a model the chat
path reports as "no model matched", e.g. after upgrading contenox or editing a
backend. Adding or removing a backend already clears its own entry; this clears
all of them at once.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx := libtracker.WithNewRequestID(context.Background())
		dbPath, err := resolveDBPath(cmd)
		if err != nil {
			return err
		}
		db, err := OpenDBAt(ctx, dbPath)
		if err != nil {
			return err
		}
		defer db.Close()

		// NewSQLiteManager wraps db without taking ownership; do not Close it.
		n, err := runtimestate.ClearModelCache(ctx, libkvstore.NewSQLiteManager(db))
		if err != nil {
			return fmt.Errorf("failed to clear model cache: %w", err)
		}
		fmt.Fprintf(cmd.OutOrStdout(), "Cleared %d cached backend model list(s). The next chat/run will refetch from each provider.\n", n)
		return nil
	},
}

func init() {
	cacheCmd.AddCommand(cacheClearCmd)
}

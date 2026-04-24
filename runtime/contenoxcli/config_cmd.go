package contenoxcli

import (
	"context"
	"fmt"
	"text/tabwriter"

	"github.com/contenox/contenox/runtime/internal/clikv"
	libdb "github.com/contenox/contenox/libdbexec"
	"github.com/contenox/contenox/libtracker"
	"github.com/contenox/contenox/runtime/runtimetypes"
	"github.com/spf13/cobra"
)

// validConfigKeys lists the keys users can set via `contenox config set`.
var validConfigKeys = map[string]string{
	"default-model":    "Default LLM model name (e.g. qwen2.5:7b)",
	"default-provider": "Default LLM provider type (e.g. ollama, openai, gemini)",
	"default-chain":    "Default chain file path (relative to .contenox/ or absolute)",
	"hitl-policy-name": "Active HITL policy file name (e.g. hitl-policy-strict.json). Empty = use hitl-policy-default.json.",
}

var configCmd = &cobra.Command{
	Use:   "config",
	Short: "Manage persistent CLI settings (default model, provider, chain, HITL policy).",
	Long: `Store and retrieve persistent CLI defaults backed by SQLite.

Global keys (shared across all projects): default-model, default-provider
Workspace keys (scoped to current project): default-chain, hitl-policy-name

Supported keys:
  default-model      Default LLM model name (e.g. qwen2.5:7b)
  default-provider   Default LLM provider type (e.g. ollama, openai, gemini)
  default-chain      Default chain file path
  hitl-policy-name   Active HITL policy file name (e.g. hitl-policy-strict.json)`,
}

var configSetCmd = &cobra.Command{
	Use:   "set <key> <value>",
	Short: "Set a persistent config value.",
	Long: `Set a persistent CLI default stored in the SQLite database.

Global keys (default-model, default-provider) are shared across all projects.
Workspace keys (default-chain, hitl-policy-name) are scoped to the current project
workspace and fall back to the global value when not set locally.

Examples:
  contenox config set default-model    qwen2.5:7b
  contenox config set default-provider ollama
  contenox config set default-chain    .contenox/default-chain.json
  contenox config set hitl-policy-name hitl-policy-strict.json`,
	Args: cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		key, value := args[0], args[1]
		if _, ok := validConfigKeys[key]; !ok {
			return fmt.Errorf("unknown key %q — valid keys: default-model, default-provider, default-chain, hitl-policy-name", key)
		}
		db, store, workspaceID, err := openConfigDBWithWorkspace(cmd)
		if err != nil {
			return err
		}
		defer db.Close()

		ctx := libtracker.WithNewRequestID(context.Background())
		if err := clikv.WriteConfig(ctx, store, workspaceID, key, value); err != nil {
			return fmt.Errorf("failed to set %q: %w", key, err)
		}
		_, scope := clikv.ReadConfig(ctx, store, workspaceID, key)
		fmt.Fprintf(cmd.OutOrStdout(), "✓  %s = %s  (%s)\n", key, value, scope)
		return nil
	},
}

var configGetCmd = &cobra.Command{
	Use:   "get <key>",
	Short: "Get a persistent config value.",
	Long: `Print the current value of a persistent CLI setting.

Examples:
  contenox config get default-model
  contenox config get default-provider
  contenox config get default-chain
  contenox config get hitl-policy-name`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		key := args[0]
		if _, ok := validConfigKeys[key]; !ok {
			return fmt.Errorf("unknown key %q", key)
		}
		db, store, workspaceID, err := openConfigDBWithWorkspace(cmd)
		if err != nil {
			return err
		}
		defer db.Close()

		ctx := libtracker.WithNewRequestID(context.Background())
		val, scope := clikv.ReadConfig(ctx, store, workspaceID, key)
		fmt.Fprintf(cmd.OutOrStdout(), "%s  (%s)\n", val, scope)
		return nil
	},
}

var configListCmd = &cobra.Command{
	Use:   "list",
	Short: "List all persistent config values.",
	Long: `Print all known CLI config keys, their current values, and their scope.

Example:
  contenox config list`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		db, store, workspaceID, err := openConfigDBWithWorkspace(cmd)
		if err != nil {
			return err
		}
		defer db.Close()

		ctx := libtracker.WithNewRequestID(context.Background())
		w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "KEY\tVALUE\tSCOPE")
		for key := range validConfigKeys {
			val, scope := clikv.ReadConfig(ctx, store, workspaceID, key)
			fmt.Fprintf(w, "%s\t%s\t%s\n", key, val, scope)
		}
		return w.Flush()
	},
}

// getConfigKV retrieves a CLI setting from the KV store, returning "" if not set.
func getConfigKV(ctx context.Context, store runtimetypes.Store, key string) (string, error) {
	return clikv.Read(ctx, store, key), nil
}

func openConfigDB(cmd *cobra.Command) (libdb.DBManager, runtimetypes.Store, error) {
	dbPath, err := resolveDBPath(cmd)
	if err != nil {
		return nil, nil, err
	}
	ctx := libtracker.WithNewRequestID(context.Background())
	db, err := OpenDBAt(ctx, dbPath)
	if err != nil {
		return nil, nil, err
	}
	return db, runtimetypes.New(db.WithoutTransaction()), nil
}

func openConfigDBWithWorkspace(cmd *cobra.Command) (libdb.DBManager, runtimetypes.Store, string, error) {
	db, store, err := openConfigDB(cmd)
	if err != nil {
		return nil, nil, "", err
	}
	contenoxDir, _ := ResolveContenoxDir(cmd)
	workspaceID := ResolveWorkspaceID(contenoxDir)
	return db, store, workspaceID, nil
}

func init() {
	configCmd.AddCommand(configSetCmd)
	configCmd.AddCommand(configGetCmd)
	configCmd.AddCommand(configListCmd)
}

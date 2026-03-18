package contenoxcli

import (
	"context"
	"encoding/json"
	"fmt"
	"text/tabwriter"

	libdb "github.com/contenox/contenox/libdbexec"
	"github.com/contenox/contenox/libtracker"
	"github.com/contenox/contenox/runtimetypes"
	"github.com/spf13/cobra"
)

// cliKVPrefix is used for all CLI-level persistent settings stored in the SQLite KV table.
const cliKVPrefix = "cli."

// validConfigKeys lists the keys users can set via `contenox config set`.
var validConfigKeys = map[string]string{
	"default-model":    "Default LLM model name (e.g. qwen2.5:7b)",
	"default-provider": "Default LLM provider type (e.g. ollama, openai, gemini)",
	"default-chain":    "Default chain file path (relative to .contenox/ or absolute)",
}

var configCmd = &cobra.Command{
	Use:   "config",
	Short: "Manage persistent CLI settings (default model, provider, chain).",
	Long: `Store and retrieve persistent CLI defaults backed by SQLite.

These settings are used when the corresponding flag is not explicitly provided.
They are project-local when .contenox/local.db exists, otherwise global (~/.contenox/local.db).

Supported keys:
  default-model      Default LLM model name (e.g. qwen2.5:7b)
  default-provider   Default LLM provider type (e.g. ollama, openai, gemini)
  default-chain      Default chain file path`,
}

var configSetCmd = &cobra.Command{
	Use:   "set <key> <value>",
	Short: "Set a persistent config value.",
	Long: `Set a persistent CLI default stored in the local SQLite database.

Valid keys:
  default-model      Default LLM model name
  default-provider   Default LLM provider type
  default-chain      Default chain file path

Examples:
  contenox config set default-model    qwen2.5:7b
  contenox config set default-provider ollama
  contenox config set default-model    gemini-2.5-flash
  contenox config set default-chain    .contenox/chain-vibes.json`,
	Args: cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		key, value := args[0], args[1]
		if _, ok := validConfigKeys[key]; !ok {
			return fmt.Errorf("unknown key %q — valid keys: default-model, default-provider, default-chain", key)
		}
		db, store, err := openConfigDB(cmd)
		if err != nil {
			return err
		}
		defer db.Close()

		ctx := libtracker.WithNewRequestID(context.Background())
		kvKey := cliKVPrefix + key
		data, _ := json.Marshal(value)
		if err := store.SetKV(ctx, kvKey, json.RawMessage(data)); err != nil {
			return fmt.Errorf("failed to set %q: %w", key, err)
		}
		fmt.Fprintf(cmd.OutOrStdout(), "Set %s = %s\n", key, value)
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
  contenox config get default-chain`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		key := args[0]
		if _, ok := validConfigKeys[key]; !ok {
			return fmt.Errorf("unknown key %q", key)
		}
		db, store, err := openConfigDB(cmd)
		if err != nil {
			return err
		}
		defer db.Close()

		ctx := libtracker.WithNewRequestID(context.Background())
		val, err := getConfigKV(ctx, store, key)
		if err != nil {
			return err
		}
		fmt.Fprintln(cmd.OutOrStdout(), val)
		return nil
	},
}

var configListCmd = &cobra.Command{
	Use:   "list",
	Short: "List all persistent config values.",
	Long: `Print all known CLI config keys and their current values.

Outputs a table of KEY and VALUE. Unset keys show an empty value.

Example:
  contenox config list`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		db, store, err := openConfigDB(cmd)
		if err != nil {
			return err
		}
		defer db.Close()

		ctx := libtracker.WithNewRequestID(context.Background())
		w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "KEY\tVALUE")
		for key := range validConfigKeys {
			val, _ := getConfigKV(ctx, store, key)
			fmt.Fprintf(w, "%s\t%s\n", key, val)
		}
		return w.Flush()
	},
}

// getConfigKV retrieves a CLI setting from the KV store, returning "" if not set.
func getConfigKV(ctx context.Context, store runtimetypes.Store, key string) (string, error) {
	var val string
	if err := store.GetKV(ctx, cliKVPrefix+key, &val); err != nil {
		return "", nil // Not set is not an error.
	}
	return val, nil
}

func openConfigDB(cmd *cobra.Command) (libdb.DBManager, runtimetypes.Store, error) {
	dbPath, err := resolveDBPath(cmd)
	if err != nil {
		return nil, nil, err
	}
	ctx := libtracker.WithNewRequestID(context.Background())
	db, err := openDBAt(ctx, dbPath)
	if err != nil {
		return nil, nil, err
	}
	return db, runtimetypes.New(db.WithoutTransaction()), nil
}

func init() {
	configCmd.AddCommand(configSetCmd)
	configCmd.AddCommand(configGetCmd)
	configCmd.AddCommand(configListCmd)
}

package contenoxcli

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/contenox/beam/internal/kernel/reasoning"
	libdb "github.com/contenox/beam/internal/libdbexec"
	"github.com/contenox/beam/internal/libtracker"
	"github.com/contenox/beam/internal/services/clikv"
	"github.com/contenox/beam/internal/store/runtimetypes"
	"github.com/spf13/cobra"
)

// validConfigKeys lists the keys users can set via `contenox config set`.
var validConfigKeys = map[string]string{
	"default-model":                 "Default LLM model name (e.g. qwen2.5:7b)",
	"default-provider":              "Default LLM provider type (e.g. ollama, openai, gemini)",
	"default-alt-model":             "Optional alt LLM model name. Used by chains referencing {{var:alt_model}}.",
	"default-alt-provider":          "Optional alt LLM provider type. Used by chains referencing {{var:alt_provider}}.",
	"default-autocomplete-model":    "Optional VS Code autocomplete model name, independent from default-model.",
	"default-autocomplete-provider": "Optional VS Code autocomplete provider type, independent from default-provider.",
	"default-max-tokens":            "Optional default response token cap. Used by chains referencing {{var:max_tokens}}.",
	"default-think":                 "Default reasoning level: auto, off, minimal, low, medium, high, xhigh.",
	"default-chain":                 "Default chain file path (relative to .contenox/ or absolute)",
	"hitl-policy-name":              "Active HITL policy file name (e.g. hitl-policy-strict.json). Empty = use hitl-policy-default.json.",
	"telemetry-enabled":             "Enable writing telemetry logs to <data-dir>/telemetry.log (true/false)",
	"update-check":                  "Enable automatic update availability checks (true/false). Set false for zero-trust/air-gapped environments.",
	"default-mission-agent":         "Default declared agent fired by '/mission <intent>' and 'contenox mission fire' with no --agent.",
	"default-mission-policy":        "Default mission envelope (HITL policy) used when '/mission' or 'contenox mission fire' names none.",
	"fleet-max-parallel":            "Fleet-width admission cap: max concurrently open mission units (integer; 0 = unlimited; default 8).",
}

var configCmd = &cobra.Command{
	Use:   "config",
	Short: "Manage persistent CLI settings (default model, provider, chain, HITL policy).",
	Long: `Store and retrieve persistent CLI defaults backed by SQLite.

Global keys (shared across all projects): default-model, default-provider, default-alt-model, default-alt-provider, default-autocomplete-model, default-autocomplete-provider, default-max-tokens, default-think, telemetry-enabled, update-check, default-mission-agent, default-mission-policy
Workspace keys (scoped to current project): default-chain, hitl-policy-name

Supported keys:
  default-model                  Default LLM model name (e.g. qwen2.5:7b)
  default-provider               Default LLM provider type (e.g. ollama, openai, gemini)
  default-alt-model              Optional alt LLM model name (chains using {{var:alt_model}})
  default-alt-provider           Optional alt LLM provider (chains using {{var:alt_provider}})
  default-autocomplete-model     Optional VS Code autocomplete model, separate from chat
  default-autocomplete-provider  Optional VS Code autocomplete provider, separate from chat
  default-max-tokens             Optional response token cap (chains using {{var:max_tokens}})
  default-think                  Default reasoning level: auto, off, minimal, low, medium, high, xhigh
  telemetry-enabled              Enable local telemetry logs (true/false)
  update-check                   Enable automatic update checks (true/false)
  default-chain                  Default chain file path
  hitl-policy-name               Active HITL policy file name (e.g. hitl-policy-strict.json)
  default-mission-agent          Default agent fired by /mission and 'mission fire' with no --agent
  default-mission-policy         Default mission envelope (HITL policy) when none is named`,
}

var configSetCmd = &cobra.Command{
	Use:   "set <key> <value>",
	Short: "Set a persistent config value.",
	Long: `Set a persistent CLI default stored in the SQLite database.

Global keys (default-model, default-provider, default-alt-model, default-alt-provider, default-autocomplete-model, default-autocomplete-provider, default-max-tokens, default-think, telemetry-enabled, update-check) are shared across all projects.
Workspace keys (default-chain, hitl-policy-name) are scoped to the current project
workspace and fall back to the global value when not set locally.

Examples:
  contenox config set default-model    qwen2.5:7b
  contenox config set default-provider ollama

  # Local-network Ollama autocomplete only:
  contenox config set default-autocomplete-provider ollama
  contenox config set default-autocomplete-model    qwen2.5-coder:7b

  contenox config set default-max-tokens 8192
  contenox config set default-think    high
  contenox config set default-chain    .contenox/default-chain.json
  contenox config set hitl-policy-name hitl-policy-strict.json`,
	Args: cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		key, value := args[0], args[1]
		if _, ok := validConfigKeys[key]; !ok {
			return fmt.Errorf("unknown key %q — valid keys: %s", key, validConfigKeyList())
		}
		if key == "default-max-tokens" {
			normalized, err := normalizeMaxTokensConfig(value)
			if err != nil {
				return err
			}
			value = normalized
		}
		if key == "default-think" {
			normalized, err := reasoning.Normalize(value)
			if err != nil {
				return err
			}
			value = normalized
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
  contenox config get default-think
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
		for _, key := range validConfigKeyNames() {
			val, scope := clikv.ReadConfig(ctx, store, workspaceID, key)
			fmt.Fprintf(w, "%s\t%s\t%s\n", key, val, scope)
		}
		return w.Flush()
	},
}

func normalizeMaxTokensConfig(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", nil
	}
	n, err := strconv.Atoi(value)
	if err != nil {
		return "", fmt.Errorf("default-max-tokens must be a non-negative integer, got %q", value)
	}
	if n < 0 {
		return "", fmt.Errorf("default-max-tokens must be non-negative, got %d", n)
	}
	return strconv.Itoa(n), nil
}

func validConfigKeyList() string {
	return strings.Join(validConfigKeyNames(), ", ")
}

func validConfigKeyNames() []string {
	keys := make([]string, 0, len(validConfigKeys))
	for key := range validConfigKeys {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	return keys
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

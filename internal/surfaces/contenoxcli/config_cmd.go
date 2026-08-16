package contenoxcli

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/contenox/contenox/internal/kernel/reasoning"
	"github.com/contenox/contenox/internal/services/clikv"
	"github.com/contenox/contenox/internal/store/runtimetypes"
	libdb "github.com/contenox/contenox/libdbexec"
	"github.com/contenox/contenox/libtracker"
	"github.com/spf13/cobra"
)

var validConfigKeys = map[string]string{
	"default-model":                 "Default LLM model name (e.g. qwen3:8b)",
	"default-provider":              "Default LLM provider type (e.g. ollama, openai, gemini)",
	"default-alt-model":             "Optional alt LLM model name. Used by chains referencing {{var:alt_model}}.",
	"default-alt-provider":          "Optional alt LLM provider type. Used by chains referencing {{var:alt_provider}}.",
	"default-autocomplete-model":    "Optional editor autocomplete model name, independent from default-model.",
	"default-autocomplete-provider": "Optional editor autocomplete provider type, independent from default-provider.",
	"default-audio-model":           "Optional model preferred for requests carrying audio attachments. Unset falls back to default-model; audio requests resolve only to audio-capable models either way.",
	"default-audio-provider":        "Optional provider type for the audio model, independent from default-provider. Unset uses default-provider.",
	"default-max-tokens":            "Optional default response token cap. Used by chains referencing {{var:max_tokens}}.",
	"default-think":                 "Default reasoning level: auto, off, minimal, low, medium, high, xhigh.",
	"default-chain":                 "Default chain file path (relative to .contenox/ or absolute)",
	"hitl-policy-name":              "Active HITL policy file name (e.g. hitl-policy-strict.json). Empty = use hitl-policy-default.json.",
	"telemetry-enabled":             "Enable writing telemetry logs to <data-dir>/telemetry.log (true/false)",
	"update-check":                  "Enable automatic update availability checks (true/false). Set false for zero-trust/air-gapped environments.",
	"opt-in-beta":                   "Enable beta features (true/false): agent roster, event triggers. CONTENOX_OPT_IN_BETA overrides per invocation.",
	"default-mission-agent":         "Default declared agent run as a subagent by '/plan', mission_start, '/mission <intent>' and 'contenox mission fire' with no --agent.",
	"default-mission-policy":        "Default subagent envelope (HITL policy) used when none is named.",
	"default-oracle-chain":          "Chain that adjudicates a subagent's asks (e.g. chain-oracle-default.json). Unset means no oracle: every ask waits for a human.",
	"default-oracle-policy":         "Envelope the oracle chain itself runs under. Unset uses hitl-policy-oracle.json.",
	"oracle-approves-tool-calls":    "Let the oracle rule on a subagent's approve-tier TOOL CALLS, not just its questions (true/false). The subagent envelope's attention.allowAgentApprovals still has to permit it.",
	"fleet-max-parallel":            "Fleet-width admission cap: max concurrently open mission units (integer; 0 = unlimited; default 8).",
	"log-max-size":                  "Start a new part of the host log once it reaches this size (e.g. 10MB, 512KB). Applies to 'contenox serve'.",
	"log-max-files":                 "How many host log files to keep, counted across every date and part (integer; 0 = unlimited).",
	"log-max-age-days":              "Delete host logs whose date is older than this many days (integer; 0 = no age limit).",
}

var configCmd = &cobra.Command{
	Use:   "config",
	Short: "Manage persistent CLI settings (default model, provider, chain, HITL policy).",
	Long: `Store and retrieve persistent CLI defaults backed by SQLite.

Global keys (shared across all projects): default-model, default-provider, default-alt-model, default-alt-provider, default-autocomplete-model, default-autocomplete-provider, default-audio-model, default-audio-provider, default-max-tokens, default-think, telemetry-enabled, update-check, opt-in-beta, default-mission-agent, default-mission-policy, log-max-size, log-max-files, log-max-age-days
Workspace keys (scoped to current project): default-chain, hitl-policy-name

Supported keys:
  default-model                  Default LLM model name (e.g. qwen3:8b)
  default-provider               Default LLM provider type (e.g. ollama, openai, gemini)
  default-alt-model              Optional alt LLM model name (chains using {{var:alt_model}})
  default-alt-provider           Optional alt LLM provider (chains using {{var:alt_provider}})
  default-autocomplete-model     Optional editor autocomplete model, separate from chat
  default-autocomplete-provider  Optional editor autocomplete provider, separate from chat
  default-audio-model            Optional model preferred for requests carrying audio
  default-audio-provider         Optional provider for the audio model, separate from default-provider
  default-max-tokens             Optional response token cap (chains using {{var:max_tokens}})
  default-think                  Default reasoning level: auto, off, minimal, low, medium, high, xhigh
  telemetry-enabled              Enable local telemetry logs (true/false)
  update-check                   Enable automatic update checks (true/false)
  opt-in-beta                    Enable beta features: agent roster, event triggers (true/false)
  default-chain                  Default chain file path
  hitl-policy-name               Active HITL policy file name (e.g. hitl-policy-strict.json)
  default-mission-agent          Default agent run as a subagent when none is named
  default-mission-policy         Default subagent envelope (HITL policy) when none is named
  default-oracle-chain           Chain that adjudicates a subagent's asks; unset means human-only
  default-oracle-policy          Envelope the oracle chain runs under (default hitl-policy-oracle.json)
  oracle-approves-tool-calls     Let the oracle rule on gated tool calls too (true/false)
  log-max-size                   Size at which 'contenox serve' starts a new log part (e.g. 10MB, 512KB)
  log-max-files                  How many host log files to keep, across every date and part (0 = unlimited)
  log-max-age-days               Delete host logs older than this many days (0 = no age limit)`,
}

var configSetCmd = &cobra.Command{
	Use:   "set <key> <value>",
	Short: "Set a persistent config value.",
	Long: `Set a persistent CLI default stored in the SQLite database.

Global keys (default-model, default-provider, default-alt-model, default-alt-provider, default-autocomplete-model, default-autocomplete-provider, default-audio-model, default-audio-provider, default-max-tokens, default-think, telemetry-enabled, update-check, opt-in-beta) are shared across all projects.
Workspace keys (default-chain, hitl-policy-name) are scoped to the current project
workspace and fall back to the global value when not set locally.

Examples:
  contenox config set default-model    qwen3:8b
  contenox config set default-provider ollama

  # Local-network Ollama autocomplete only:
  contenox config set default-autocomplete-provider ollama
  contenox config set default-autocomplete-model    qwen2.5-coder:7b

  contenox config set default-max-tokens 8192
  contenox config set default-think    high
  contenox config set default-chain    .contenox/.generated/chain-agent-acp.json
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
		// Validated here rather than at read time, while the person who typed it
		// is still watching.
		if normalized, err := normalizeLogConfig(key, value); err != nil {
			return err
		} else if normalized != "" {
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

package contenoxcli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"

	"github.com/contenox/contenox/internal/kernel/agentinstance"
	"github.com/contenox/contenox/internal/models/backendservice"
	"github.com/contenox/contenox/internal/models/modelrepo"
	"github.com/contenox/contenox/internal/models/modelrepo/scriptedtest"
	"github.com/contenox/contenox/internal/models/runtimestate"
	"github.com/contenox/contenox/internal/store/runtimetypes"
	"github.com/contenox/contenox/internal/substrate"
	libdb "github.com/contenox/contenox/libdbexec"
	"github.com/contenox/contenox/libtracker"
	"github.com/google/uuid"
	"github.com/spf13/cobra"
)

func invalidateBackendModelCache(ctx context.Context, errW io.Writer, db libdb.DBManager, backendID string) {
	kv, releaseKV, err := substrate.OpenKV(ctx, db)
	if err != nil {
		warnModelCacheKept(errW, backendID, err)
		return
	}
	defer releaseKV()
	if err := runtimestate.InvalidateModelCache(ctx, kv, backendID); err != nil {
		warnModelCacheKept(errW, backendID, err)
	}
}

func warnModelCacheKept(errW io.Writer, backendID string, err error) {
	fmt.Fprintf(errW, "warning: model cache invalidation failed for backend %s: %v\n", backendID, err)
	fmt.Fprintf(errW, "         the backend change is saved; run 'contenox cache clear' to drop the stale model list\n")
}

var backendCmd = &cobra.Command{
	Use:   "backend",
	Short: "Manage LLM backends (add, list, show, remove).",
	Long: `Register and manage LLM backend endpoints.

A backend points at an LLM provider. Supported types:
  ollama                        Local Ollama daemon (requires: ollama serve) or hosted Ollama Cloud.
  openai                        api.openai.com (requires --api-key-env).
  gemini                        Google Gemini (requires --api-key-env).
  vllm                          Self-hosted OpenAI-compatible endpoint (requires --url).
  vertex-google                 Google Cloud Vertex AI / Gemini (requires gcloud auth application-default
                                login and GOOGLE_CLOUD_PROJECT).
  scripted-test                 TEST ONLY. Replays a scripted dialog from a JSON file (--script). No model
                                is ever called; every reply is the next turn in the file.

Examples:
  # Register a local Ollama server (default URL inferred):
  contenox backend add ollama --type ollama

  # Register Ollama Cloud directly:
  contenox backend add ollama-cloud --type ollama --url https://ollama.com/api --api-key-env OLLAMA_API_KEY

  # Register OpenAI using an environment variable for the key:
  contenox backend add openai --type openai --api-key-env OPENAI_API_KEY

  # Register Google Gemini:
  contenox backend add gemini --type gemini --api-key-env GEMINI_API_KEY

  # Register a Google Vertex AI backend (run gcloud auth application-default login first):
  export GOOGLE_CLOUD_PROJECT=my-project-id
  contenox backend add vertex --type vertex-google \
    --url "https://aiplatform.googleapis.com/v1/projects/$GOOGLE_CLOUD_PROJECT/locations/global"
  # or regional (data residency): --url "https://{REGION}-aiplatform.googleapis.com/v1/projects/$GOOGLE_CLOUD_PROJECT/locations/{REGION}"
  # Model availability differs per endpoint — check with: contenox model list

  # Register a custom vLLM server:
  contenox backend add myvllm --type vllm --url http://gpu-host:8000

  # Register the scripted TEST backend (replays a dialog; never calls a model):
  contenox backend add scripted --type scripted-test --script ./dialog.json

  contenox backend list
  contenox backend show openai
  contenox backend remove myvllm`,
}

// defaultBaseURLForType infers --url for backend types with one sensible
// default, errors for types carrying account-specific info, and returns ""
// for the rest, where the caller must pass --url.
func defaultBaseURLForType(typ string) (string, error) {
	switch typ {
	case "ollama":
		return "http://localhost:11434", nil
	case "openai":
		return "https://api.openai.com/v1", nil
	case "anthropic":
		return "https://api.anthropic.com", nil
	case "gemini":
		return "https://generativelanguage.googleapis.com", nil
	case "vertex-google":
		return "", fmt.Errorf("--url is required for %s backends\n  Include project and location — global endpoint:\n  --url \"https://aiplatform.googleapis.com/v1/projects/$GOOGLE_CLOUD_PROJECT/locations/global\"\n  or regional (data residency):\n  --url \"https://{REGION}-aiplatform.googleapis.com/v1/projects/$GOOGLE_CLOUD_PROJECT/locations/{REGION}\"\n  Model availability differs per endpoint (contenox model list)", typ)
	case "bedrock":
		return "", fmt.Errorf("--url is required for bedrock backends (it carries the region)\n  e.g.: --url \"https://bedrock-runtime.us-east-1.amazonaws.com\"\n  Credentials come from the ambient AWS chain (env / profile / IAM role); no --api-key needed unless storing static keys.")
	case modelrepo.ScriptedTestBackendType:
		return "", fmt.Errorf("--script is required for %s backends (it carries the dialog file)\n  e.g.: --script ./dialog.json", modelrepo.ScriptedTestBackendType)
	default:
		return "", nil
	}
}

var backendAddCmd = &cobra.Command{
	Use:   "add <name>",
	Short: "Register an LLM backend endpoint.",
	Long: `Register a named LLM backend endpoint in the local SQLite database.

The --type flag determines which provider protocol is used.
  openai, anthropic,
  gemini                        Cloud providers. Base URL inferred if --url is omitted. Requires --api-key-env.
  ollama                        Local daemon (requires 'ollama serve') or hosted Ollama Cloud (use
                                --url https://ollama.com/api and --api-key-env OLLAMA_API_KEY).
  vllm                          Self-hosted OpenAI-compatible endpoint (requires --url).
  vertex-google                 Google Cloud Vertex AI / Gemini (requires gcloud auth application-default login).
  scripted-test                 TEST ONLY. Replays a scripted dialog from a JSON file (requires --script).

API keys should be passed via --api-key-env (reads from environment) rather than
--api-key (inline literal) to avoid leaking secrets into shell history.

The scripted-test type is a fake: it calls no model and replays the turns in its
--script file in order, one per model turn. Every surface that names the active
provider shows "scripted-test", and 'contenox doctor' warns while it is the default.
It proves the machinery (chain loop, tool dispatch, HITL gate), not the agent's
judgement — you wrote the tool calls, so no real model was asked to choose them.

Examples:
  contenox backend add ollama     --type ollama
  contenox backend add ollama-cloud --type ollama    --url https://ollama.com/api --api-key-env OLLAMA_API_KEY
  contenox backend add openai     --type openai      --api-key-env OPENAI_API_KEY
  contenox backend add gemini     --type gemini      --api-key-env GEMINI_API_KEY
  contenox backend add myvllm    --type vllm         --url http://gpu-host:8000
  contenox backend add scripted  --type scripted-test --script ./dialog.json`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx := libtracker.WithNewRequestID(context.Background())
		name := args[0]
		flags := cmd.Flags()

		typ, _ := flags.GetString("type")
		baseURL, _ := flags.GetString("url")
		scriptPath, _ := flags.GetString("script")
		apiKeyEnv, _ := flags.GetString("api-key-env")
		apiKeyLit, _ := flags.GetString("api-key")

		typ = strings.ToLower(strings.TrimSpace(typ))
		if typ == "" {
			typ = "ollama"
		}
		if scriptPath = strings.TrimSpace(scriptPath); scriptPath != "" {
			if typ != modelrepo.ScriptedTestBackendType {
				return fmt.Errorf("--script only applies to --type %s backends", modelrepo.ScriptedTestBackendType)
			}
			abs, err := scriptedtest.ResolvePath(scriptPath)
			if err != nil {
				return err
			}
			if _, err := scriptedtest.Load(abs); err != nil {
				return err
			}
			baseURL = abs
		}
		if baseURL == "" {
			inferred, err := defaultBaseURLForType(typ)
			if err != nil {
				return err
			}
			baseURL = inferred
		}
		apiKey := apiKeyLit
		if apiKey == "" && apiKeyEnv != "" {
			apiKey = os.Getenv(apiKeyEnv)
		}

		// A double-slash in the path is almost always an un-expanded environment
		// variable such as an empty $GOOGLE_CLOUD_PROJECT. A scripted-test base
		// URL is a filesystem path, not a URL, so the heuristic does not apply.
		if baseURL != "" && typ != modelrepo.ScriptedTestBackendType {
			pathPart := baseURL
			if idx := strings.Index(baseURL, "://"); idx >= 0 {
				pathPart = baseURL[idx+3:] // skip "https://"
			}
			if strings.Contains(pathPart, "//") {
				return fmt.Errorf(
					"--url %q looks malformed (consecutive slashes in path).\n"+
						"  This usually means an environment variable like $GOOGLE_CLOUD_PROJECT was not set.\n"+
						"  Export it and retry:\n"+
						"    export GOOGLE_CLOUD_PROJECT=my-project-id",
					baseURL)
			}
		}

		db, svc, err := openBackendDB(cmd)
		if err != nil {
			return err
		}
		defer db.Close()

		backend := &runtimetypes.Backend{
			ID:      uuid.NewString(),
			Name:    name,
			Type:    typ,
			BaseURL: baseURL,
		}
		if err := svc.Create(ctx, backend); err != nil {
			return fmt.Errorf("failed to add backend: %w", err)
		}

		if apiKey != "" {
			if err := setProviderConfigKV(ctx, runtimetypes.New(db.WithoutTransaction()), typ, apiKey); err != nil {
				return fmt.Errorf("backend added but failed to store API key: %w", err)
			}
		}

		// Drop any stale cached model list so the next chat/run refetches.
		invalidateBackendModelCache(ctx, cmd.ErrOrStderr(), db, backend.ID)

		fmt.Fprintf(cmd.OutOrStdout(), "Backend %q added (%s → %s).\n", name, typ, baseURL)
		if typ == modelrepo.ScriptedTestBackendType {
			fmt.Fprintf(cmd.OutOrStdout(), "WARNING: %s is a TEST backend. It calls no model — every reply is replayed from %s in order.\n", modelrepo.ScriptedTestBackendType, baseURL)
			fmt.Fprintf(cmd.OutOrStdout(), "         Point the defaults at it with:\n           contenox config set default-provider %s\n           contenox config set default-model %s\n", modelrepo.ScriptedTestBackendType, scriptedTestModelName(baseURL))
		}
		return nil
	},
}

var backendListCmd = &cobra.Command{
	Use:   "list",
	Short: "List all registered backends.",
	Long: `List every registered backend as a table of name, type, and URL.

If no backends are registered, prints a hint to run 'contenox backend add'.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx := libtracker.WithNewRequestID(context.Background())
		db, svc, err := openBackendDB(cmd)
		if err != nil {
			return err
		}
		defer db.Close()

		backends, err := svc.List(ctx, nil, 100)
		if err != nil {
			return fmt.Errorf("failed to list backends: %w", err)
		}
		if len(backends) == 0 {
			fmt.Fprintln(cmd.OutOrStdout(), "No backends registered. Run: contenox backend add <name> --type <type>")
			return nil
		}
		w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "NAME\tTYPE\tURL")
		for _, b := range backends {
			fmt.Fprintf(w, "%s\t%s\t%s\n", b.Name, b.Type, b.BaseURL)
		}
		return w.Flush()
	},
}

var backendShowCmd = &cobra.Command{
	Use:   "show <name>",
	Short: "Show details for a backend.",
	Long: `Look up a backend by name and print its full record as indented JSON,
including its id, type, and base URL.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx := libtracker.WithNewRequestID(context.Background())
		db, _, err := openBackendDB(cmd)
		if err != nil {
			return err
		}
		defer db.Close()

		store := runtimetypes.New(db.WithoutTransaction())
		b, err := store.GetBackendByName(ctx, args[0])
		if err != nil {
			return fmt.Errorf("backend %q not found: %w", args[0], err)
		}
		data, _ := json.MarshalIndent(b, "", "  ")
		fmt.Fprintln(cmd.OutOrStdout(), string(data))
		return nil
	},
}

var backendRemoveCmd = &cobra.Command{
	Use:     "remove <name>",
	Aliases: []string{"rm", "delete"},
	Short:   "Remove a registered backend.",
	Long: `Delete a backend by name from the local database and drop its cached model
list so the next chat or run no longer sees it. This removes only the local
registration; it does not affect the remote provider or endpoint.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx := libtracker.WithNewRequestID(context.Background())
		db, svc, err := openBackendDB(cmd)
		if err != nil {
			return err
		}
		defer db.Close()

		store := runtimetypes.New(db.WithoutTransaction())
		b, err := store.GetBackendByName(ctx, args[0])
		if err != nil {
			return fmt.Errorf("backend %q not found: %w", args[0], err)
		}
		if err := svc.Delete(ctx, b.ID); err != nil {
			return fmt.Errorf("failed to remove backend: %w", err)
		}

		// Drop the removed backend's cached model list.
		invalidateBackendModelCache(ctx, cmd.ErrOrStderr(), db, b.ID)

		fmt.Fprintf(cmd.OutOrStdout(), "Backend %q removed.\n", args[0])
		return nil
	},
}

func scriptedTestModelName(scriptPath string) string {
	script, err := scriptedtest.Load(scriptPath)
	if err != nil {
		return scriptedtest.DefaultModelName
	}
	return script.Model
}

func openBackendDB(cmd *cobra.Command) (libdb.DBManager, backendservice.Service, error) {
	dbPath, err := resolveDBPath(cmd)
	if err != nil {
		return nil, nil, err
	}
	ctx := libtracker.WithNewRequestID(context.Background())
	db, err := OpenDBAt(ctx, dbPath)
	if err != nil {
		return nil, nil, err
	}
	return db, backendservice.New(db), nil
}

func resolveDBPath(cmd *cobra.Command) (string, error) {
	dbFlag, _ := cmd.Flags().GetString("db")
	if dbFlag == "" {
		dbFlag, _ = cmd.Root().PersistentFlags().GetString("db")
	}
	if dbFlag != "" {
		return filepath.Abs(dbFlag)
	}
	// A unit dispatched by another contenox process inherits its parent's
	// database. An explicit --db still wins.
	if inherited := strings.TrimSpace(os.Getenv(agentinstance.ChainDBEnvVar)); inherited != "" {
		return filepath.Abs(inherited)
	}
	return globalDBPath()
}

func globalDBPath() (string, error) {
	dir, err := globalContenoxDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "local.db"), nil
}

func globalContenoxDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("cannot determine home directory: %w", err)
	}
	dir := filepath.Join(home, ".contenox")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("create ~/.contenox: %w", err)
	}
	return dir, nil
}

func init() {
	backendAddCmd.Flags().String("type", "ollama", "Backend type: ollama, openai, anthropic, bedrock, gemini, vllm, vertex-google")
	backendAddCmd.Flags().String("url", "", "Base URL of the backend (auto-inferred for openai/anthropic/gemini if omitted; set https://ollama.com/api for hosted Ollama)")
	backendAddCmd.Flags().String("script", "", "Path to the dialog file for a --type scripted-test backend (TEST ONLY: replays turns instead of calling a model)")
	backendAddCmd.Flags().String("api-key-env", "", "Name of the environment variable holding the API key (preferred over --api-key)")
	backendAddCmd.Flags().String("api-key", "", "API key literal — prefer --api-key-env to avoid leaking into shell history")

	backendCmd.AddCommand(backendAddCmd)
	backendCmd.AddCommand(backendListCmd)
	backendCmd.AddCommand(backendShowCmd)
	backendCmd.AddCommand(backendRemoveCmd)
}

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
	"github.com/contenox/contenox/internal/models/runtimestate"
	"github.com/contenox/contenox/internal/store/runtimetypes"
	libdb "github.com/contenox/contenox/libdbexec"
	"github.com/contenox/contenox/libkvstore"
	"github.com/contenox/contenox/libtracker"
	"github.com/google/uuid"
	"github.com/spf13/cobra"
)

// invalidateBackendModelCache busts the cached model list (prov:<id>) for a
// backend so the next chat/run refetches from the provider. Best-effort: a
// failure is surfaced as a warning but does not fail the backend operation.
// NewSQLiteManager wraps db without taking ownership, so it is not closed here.
func invalidateBackendModelCache(ctx context.Context, errW io.Writer, db libdb.DBManager, backendID string) {
	if err := runtimestate.InvalidateModelCache(ctx, libkvstore.NewSQLiteManager(db), backendID); err != nil {
		fmt.Fprintf(errW, "warning: model cache invalidation failed for backend %s: %v\n", backendID, err)
	}
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

  contenox backend list
  contenox backend show openai
  contenox backend remove myvllm`,
}

// defaultBaseURLForType infers --url for backend types with one sensible
// default, and errors out for types where a default would be actively wrong
// (vertex-google/bedrock carry account-specific info no default could guess).
// Types not listed here (vllm, myvllm-style custom endpoints) return ""
// unchanged — the caller must pass --url explicitly.
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

API keys should be passed via --api-key-env (reads from environment) rather than
--api-key (inline literal) to avoid leaking secrets into shell history.

Examples:
  contenox backend add ollama     --type ollama
  contenox backend add ollama-cloud --type ollama    --url https://ollama.com/api --api-key-env OLLAMA_API_KEY
  contenox backend add openai     --type openai      --api-key-env OPENAI_API_KEY
  contenox backend add gemini     --type gemini      --api-key-env GEMINI_API_KEY
  contenox backend add myvllm    --type vllm         --url http://gpu-host:8000`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx := libtracker.WithNewRequestID(context.Background())
		name := args[0]
		flags := cmd.Flags()

		typ, _ := flags.GetString("type")
		baseURL, _ := flags.GetString("url")
		apiKeyEnv, _ := flags.GetString("api-key-env")
		apiKeyLit, _ := flags.GetString("api-key")

		typ = strings.ToLower(strings.TrimSpace(typ))
		if typ == "" {
			typ = "ollama"
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

		// Sanity-check the URL: a double-slash in the path (after stripping the scheme)
		// is almost always caused by an un-expanded environment variable such as
		// $GOOGLE_CLOUD_PROJECT being empty.  Catch it early rather than silently
		// registering a broken backend.
		if baseURL != "" {
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
	// A unit dispatched by another contenox process inherits its parent's database,
	// so its mission writes land on the row that dispatched it. An explicit --db
	// still wins; see agentinstance.ChainDBEnvVar.
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
	backendAddCmd.Flags().String("api-key-env", "", "Name of the environment variable holding the API key (preferred over --api-key)")
	backendAddCmd.Flags().String("api-key", "", "API key literal — prefer --api-key-env to avoid leaking into shell history")

	backendCmd.AddCommand(backendAddCmd)
	backendCmd.AddCommand(backendListCmd)
	backendCmd.AddCommand(backendShowCmd)
	backendCmd.AddCommand(backendRemoveCmd)
}

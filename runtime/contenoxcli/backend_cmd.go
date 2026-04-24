package contenoxcli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"

	"github.com/contenox/contenox/runtime/backendservice"
	libdb "github.com/contenox/contenox/libdbexec"
	"github.com/contenox/contenox/libtracker"
	"github.com/contenox/contenox/runtime/runtimetypes"
	"github.com/google/uuid"
	"github.com/spf13/cobra"
)

var backendCmd = &cobra.Command{
	Use:   "backend",
	Short: "Manage LLM backends (add, list, show, remove).",
	Long: `Register and manage LLM backend endpoints.

A backend points at an LLM provider. Supported types:
  local                         Embedded llama.cpp inference — NO external server, NO network, NO API key.
                                The contenox binary runs the model in-process on CPU (or GPU where available).
                                Point --url at a local GGUF file or a huggingface.co URL.
  ollama                        Local Ollama daemon (requires: ollama serve) or hosted Ollama Cloud.
  openai                        api.openai.com (requires --api-key-env).
  gemini                        Google Gemini (requires --api-key-env).
  vllm                          Self-hosted OpenAI-compatible endpoint (requires --url).
  vertex-google / -anthropic    Google Cloud Vertex AI (requires gcloud auth application-default login
  / -meta / -mistralai          and GOOGLE_CLOUD_PROJECT).

Examples:
  # Fully embedded inference — no daemon, no network:
  contenox backend add local --type local --url <path-to-gguf-or-huggingface-url>

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
    --url "https://us-central1-aiplatform.googleapis.com/v1/projects/$GOOGLE_CLOUD_PROJECT/locations/us-central1"

  # Register a custom vLLM server:
  contenox backend add myvllm --type vllm --url http://gpu-host:8000

  contenox backend list
  contenox backend show openai
  contenox backend remove myvllm`,
}

var backendAddCmd = &cobra.Command{
	Use:   "add <name>",
	Short: "Register an LLM backend endpoint.",
	Long: `Register a named LLM backend endpoint in the local SQLite database.

The --type flag determines which provider protocol is used.
  local                         Embedded llama.cpp inference compiled into the contenox binary.
                                No Ollama, no external server, no API key required. Pass --url with the
                                path to a GGUF file or a huggingface.co URL.
  openai, gemini                Cloud providers. Base URL inferred if --url is omitted. Requires --api-key-env.
  ollama                        Local daemon (requires 'ollama serve') or hosted Ollama Cloud (use
                                --url https://ollama.com/api and --api-key-env OLLAMA_API_KEY).
  vllm                          Self-hosted OpenAI-compatible endpoint (requires --url).
  vertex-google / -anthropic    Google Cloud Vertex AI (requires gcloud auth application-default login).
  / -meta / -mistralai

API keys should be passed via --api-key-env (reads from environment) rather than
--api-key (inline literal) to avoid leaking secrets into shell history.

Examples:
  contenox backend add embedded --type local  --url <path-or-hf-url>
  contenox backend add ollama  --type ollama
  contenox backend add ollama-cloud --type ollama --url https://ollama.com/api --api-key-env OLLAMA_API_KEY
  contenox backend add openai  --type openai  --api-key-env OPENAI_API_KEY
  contenox backend add gemini  --type gemini  --api-key-env GEMINI_API_KEY
  contenox backend add myvllm --type vllm    --url http://gpu-host:8000`,
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
			switch typ {
			case "ollama":
				baseURL = "http://localhost:11434"
			case "openai":
				baseURL = "https://api.openai.com/v1"
			case "gemini":
				baseURL = "https://generativelanguage.googleapis.com"
			case "vertex-google", "vertex-anthropic", "vertex-meta", "vertex-mistralai":
				return fmt.Errorf("--url is required for %s backends\n  Include project and location, e.g.:\n  --url \"https://us-central1-aiplatform.googleapis.com/v1/projects/$GOOGLE_CLOUD_PROJECT/locations/us-central1\"", typ)
			}
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

		fmt.Fprintf(cmd.OutOrStdout(), "Backend %q added (%s → %s).\n", name, typ, baseURL)
		return nil
	},
}

var backendListCmd = &cobra.Command{
	Use:   "list",
	Short: "List all registered backends.",
	Args:  cobra.NoArgs,
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
	Args:  cobra.ExactArgs(1),
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
	Args:    cobra.ExactArgs(1),
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
	return globalDBPath()
}

func globalDBPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("cannot determine home directory: %w", err)
	}
	dir := filepath.Join(home, ".contenox")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("create ~/.contenox: %w", err)
	}
	return filepath.Join(dir, "local.db"), nil
}

func init() {
	backendAddCmd.Flags().String("type", "ollama", "Backend type: local (embedded llama.cpp, no external server), ollama, openai, gemini, vllm, vertex-google, vertex-anthropic, vertex-meta, vertex-mistralai")
	backendAddCmd.Flags().String("url", "", "Base URL of the backend (auto-inferred for openai/gemini if omitted; set https://ollama.com/api for hosted Ollama)")
	backendAddCmd.Flags().String("api-key-env", "", "Name of the environment variable holding the API key (preferred over --api-key)")
	backendAddCmd.Flags().String("api-key", "", "API key literal — prefer --api-key-env to avoid leaking into shell history")

	backendCmd.AddCommand(backendAddCmd)
	backendCmd.AddCommand(backendListCmd)
	backendCmd.AddCommand(backendShowCmd)
	backendCmd.AddCommand(backendRemoveCmd)
}

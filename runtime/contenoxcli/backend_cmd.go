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

A backend points at a running LLM server (Ollama, OpenAI, Gemini, vLLM, etc).
Once registered, the runtime uses it for model resolution during chain execution.

Examples:
  # Register a local Ollama server (default URL inferred):
  contenox backend add local --type ollama

  # Register Ollama Cloud directly:
  contenox backend add ollama-cloud --type ollama --url https://ollama.com/api --api-key-env OLLAMA_API_KEY

  # Register OpenAI using an environment variable for the key:
  contenox backend add openai --type openai --api-key-env OPENAI_API_KEY

  # Register Google Gemini:
  contenox backend add gemini --type gemini --api-key-env GEMINI_API_KEY

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
For openai and gemini, the base URL is inferred automatically if --url is omitted.
For hosted Ollama, use --url https://ollama.com/api together with --api-key-env OLLAMA_API_KEY.

API keys should be passed via --api-key-env (reads from environment) rather than
--api-key (inline literal) to avoid leaking secrets into shell history.

Examples:
  contenox backend add local   --type ollama
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
	Aliases: []string{"rm"},
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

// resolveDBPath finds the database path from the --db flag then falls back to well-known defaults.
func resolveDBPath(cmd *cobra.Command) (string, error) {
	dbFlag, _ := cmd.Flags().GetString("db")
	if dbFlag == "" {
		dbFlag, _ = cmd.Root().PersistentFlags().GetString("db")
	}
	if dbFlag != "" {
		return filepath.Abs(dbFlag)
	}
	// Prefer project-local DB if a .contenox directory exists (even if local.db
	// doesn't yet — e.g. right after `contenox init`).
	localDir, err := ResolveContenoxDir(cmd)
	if err == nil {
		if _, statErr := os.Stat(localDir); statErr == nil {
			return filepath.Join(localDir, "local.db"), nil
		}
	}
	if home, err := os.UserHomeDir(); err == nil {
		globalDB := filepath.Join(home, ".contenox", "local.db")
		if _, err := os.Stat(globalDB); err == nil {
			return globalDB, nil
		}
	}
	// Default: create in local .contenox/ (even if it's the cwd fallback)
	if localDir == "" {
		cwd, _ := os.Getwd()
		localDir = filepath.Join(cwd, ".contenox")
	}
	return filepath.Abs(filepath.Join(localDir, "local.db"))
}

func init() {
	backendAddCmd.Flags().String("type", "ollama", "Backend type: ollama, openai, gemini, local, vllm, vertex-google, vertex-anthropic, vertex-meta, vertex-mistralai")
	backendAddCmd.Flags().String("url", "", "Base URL of the backend (auto-inferred for openai/gemini if omitted; set https://ollama.com/api for hosted Ollama)")
	backendAddCmd.Flags().String("api-key-env", "", "Name of the environment variable holding the API key (preferred over --api-key)")
	backendAddCmd.Flags().String("api-key", "", "API key literal — prefer --api-key-env to avoid leaking into shell history")

	backendCmd.AddCommand(backendAddCmd)
	backendCmd.AddCommand(backendListCmd)
	backendCmd.AddCommand(backendShowCmd)
	backendCmd.AddCommand(backendRemoveCmd)
}

package contenoxcli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	libdb "github.com/contenox/contenox/libdbexec"
	"github.com/contenox/contenox/libtracker"
	"github.com/contenox/contenox/internal/models/backendservice"
	"github.com/contenox/contenox/internal/models/runtimestate"
	"github.com/contenox/contenox/internal/services/clikv"
	"github.com/contenox/contenox/internal/services/setupcheck"
	"github.com/contenox/contenox/internal/store/runtimetypes"
	"github.com/google/uuid"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

var setupCmd = &cobra.Command{
	Use:   "setup",
	Short: "Interactive wizard to configure your LLM provider and model.",
	Long: `Run the setup wizard to pick an LLM provider (Ollama, OpenAI,
Gemini, or Vertex AI), enter credentials, and set defaults. This is the same
wizard that runs inside IDE terminals via ACP.

Examples:
  contenox setup`,
	RunE: func(cmd *cobra.Command, args []string) error {
		return runSetup(cmd, cmd.OutOrStdout())
	},
}

type setupProvider struct {
	key          string
	label        string
	defaultModel string
	envKey       string
	needsAPIKey  bool
	// needsBaseURL marks providers whose endpoint is account-specific and cannot
	// be defaulted (e.g. Vertex, whose URL carries the GCP project + region).
	needsBaseURL bool
	baseURLHint  string
}

// setupProviders is the terminal wizard's provider menu; it duplicates
// providerservice.providerDefaultsByType, so keep the two in sync.
var setupProviders = []setupProvider{
	{key: "ollama", label: "Ollama (local daemon)", defaultModel: "qwen2.5:7b", needsAPIKey: false},
	{key: "openai", label: "OpenAI", defaultModel: "gpt-5-mini", envKey: "OPENAI_API_KEY", needsAPIKey: true},
	{key: "gemini", label: "Google Gemini", defaultModel: "gemini-flash-latest", envKey: "GEMINI_API_KEY", needsAPIKey: true},
	{key: "vertex-google", label: "Google Vertex AI (Gemini via gcloud ADC)", defaultModel: "gemini-flash-latest", needsAPIKey: false, needsBaseURL: true, baseURLHint: "https://us-central1-aiplatform.googleapis.com/v1/projects/YOUR_PROJECT/locations/us-central1"},
}

// errSetupNoInput is returned when the interactive provider prompt hits EOF, so
// setup exits non-zero instead of silently committing a default.
var errSetupNoInput = errors.New("setup: no input received (interactive); nothing changed")

func runSetup(cmd *cobra.Command, out io.Writer) error {
	fmt.Fprintln(out, "")
	fmt.Fprintln(out, "  Welcome to Contenox!")
	fmt.Fprintln(out, "")

	if err := RunGlobalInit(out); err != nil {
		return fmt.Errorf("global init: %w", err)
	}

	// Show current configuration status.
	alreadyConfigured := false
	if dbPath, gpErr := globalDBPath(); gpErr == nil {
		if db, openErr := OpenDBAt(libtracker.WithNewRequestID(context.Background()), dbPath); openErr == nil {
			store := runtimetypes.New(db.WithoutTransaction())
			ctx := libtracker.WithNewRequestID(context.Background())
			curProvider := clikv.Read(ctx, store, "default-provider")
			curModel := clikv.Read(ctx, store, "default-model")
			svc := backendservice.New(db)
			backends, _ := svc.List(ctx, nil, 100)
			db.Close()

			if curProvider != "" || curModel != "" || len(backends) > 0 {
				alreadyConfigured = true
				fmt.Fprintln(out, "")
				fmt.Fprintln(out, "  Current configuration:")
				if curProvider != "" {
					fmt.Fprintf(out, "    Provider: %s\n", curProvider)
				}
				if curModel != "" {
					fmt.Fprintf(out, "    Model:    %s\n", curModel)
				}
				if len(backends) > 0 {
					fmt.Fprintf(out, "    Backends: %d registered", len(backends))
					var names []string
					for _, b := range backends {
						names = append(names, b.Name)
					}
					fmt.Fprintf(out, " (%s)\n", strings.Join(names, ", "))
				}
			}
		}
	}

	fmt.Fprintln(out, "")
	fmt.Fprintln(out, "  Choose your LLM provider:")
	fmt.Fprintln(out, "")
	for i, p := range setupProviders {
		fmt.Fprintf(out, "    %d. %s\n", i+1, p.label)
	}
	if alreadyConfigured {
		fmt.Fprintln(out, "    q. Keep current configuration")
	}
	fmt.Fprintln(out, "")

	scanner := bufio.NewScanner(os.Stdin)
	chosen := promptChoiceOrQuit(out, scanner, "  Provider", len(setupProviders), alreadyConfigured)
	if chosen == promptEOF {
		// Refuse to commit a guessed default on a piped/non-interactive run.
		fmt.Fprintln(out, "")
		fmt.Fprintln(out, "  No input received — `contenox setup` is interactive and made no changes.")
		fmt.Fprintln(out, "  Run it in a terminal, or configure non-interactively with `contenox config set`")
		fmt.Fprintln(out, "  (e.g. `contenox config set default-provider ollama`).")
		fmt.Fprintln(out, "")
		return errSetupNoInput
	}
	if chosen < 0 {
		fmt.Fprintln(out, "  ✓ Keeping current configuration.")
		fmt.Fprintln(out, "")
		return nil
	}
	sp := setupProviders[chosen]

	var apiKey string
	if sp.needsAPIKey {
		apiKey = os.Getenv(sp.envKey)
		if apiKey != "" {
			fmt.Fprintf(out, "  ✓ Found %s in environment.\n\n", sp.envKey)
		} else {
			// Check if a key is already stored in the database.
			if dbPath, gpErr := globalDBPath(); gpErr == nil {
				if db, openErr := OpenDBAt(libtracker.WithNewRequestID(context.Background()), dbPath); openErr == nil {
					store := runtimetypes.New(db.WithoutTransaction())
					var cfg runtimestate.ProviderConfig
					kvKey := runtimestate.ProviderKeyPrefix + sp.key
					if err := store.GetKV(libtracker.WithNewRequestID(context.Background()), kvKey, &cfg); err == nil && cfg.APIKey != "" {
						apiKey = cfg.APIKey
						fmt.Fprintf(out, "  ✓ %s API key already stored.\n\n", sp.label)
					}
					db.Close()
				}
			}
		}
		if apiKey == "" {
			fmt.Fprintf(out, "  Enter your %s API key (or set %s):\n", sp.label, sp.envKey)
			apiKey = promptSecret(out, scanner, "  API key")
			if apiKey == "" {
				return fmt.Errorf("API key is required for %s", sp.label)
			}
			fmt.Fprintln(out, "")
		}
	}

	var baseURL string
	if sp.needsBaseURL {
		if sp.key == "vertex-google" {
			fmt.Fprintln(out, "  Vertex AI authenticates with Google Cloud Application Default Credentials.")
			fmt.Fprintln(out, "  If you have not already, run:")
			fmt.Fprintln(out, "    gcloud auth application-default login --project YOUR_PROJECT")
			fmt.Fprintln(out, "")
		}
		fmt.Fprintf(out, "  Enter the %s endpoint URL (with your project and location):\n", sp.label)
		if sp.baseURLHint != "" {
			fmt.Fprintf(out, "    e.g. %s\n", sp.baseURLHint)
		}
		baseURL = promptLine(out, scanner, "  URL", "")
		if baseURL == "" {
			return fmt.Errorf("an endpoint URL is required for %s", sp.label)
		}
		fmt.Fprintln(out, "")
	}

	model := sp.defaultModel
	switch sp.key {
	case "ollama":
		model = promptOllamaModel(out, scanner, model)
	default:
		model = promptLine(out, scanner, fmt.Sprintf("  Model [%s]", model), model)
	}

	dbPath, err := globalDBPath()
	if err != nil {
		return fmt.Errorf("resolve db: %w", err)
	}
	ctx := libtracker.WithNewRequestID(context.Background())
	db, err := OpenDBAt(ctx, dbPath)
	if err != nil {
		return fmt.Errorf("open db: %w", err)
	}
	defer db.Close()

	if err := registerSetupBackend(ctx, db, sp.key, apiKey, baseURL); err != nil {
		return err
	}

	store := runtimetypes.New(db.WithoutTransaction())
	_ = clikv.WriteConfig(ctx, store, "global", "default-provider", sp.key)
	if model != "" {
		_ = clikv.WriteConfig(ctx, store, "global", "default-model", model)
	}

	fmt.Fprintln(out, "")
	fmt.Fprintf(out, "  ✓ Provider: %s\n", sp.key)
	if model != "" {
		fmt.Fprintf(out, "  ✓ Model:    %s\n", model)
	}

	reportSetupReadiness(ctx, cmd, db, out, sp.key, model)
	return nil
}

// reportSetupReadiness runs the same read-only reachability check as
// `contenox doctor` and prints the verdict; it never mutates saved config.
func reportSetupReadiness(ctx context.Context, cmd *cobra.Command, db libdb.DBManager, out io.Writer, provider, model string) {
	contenoxDir, _ := ResolveContenoxDir(cmd)
	opts, err := buildRunOpts(cmd, db, contenoxDir)
	if err != nil {
		fmt.Fprintf(out, "  ⚠  Could not verify setup: %v\n", err)
		fmt.Fprintln(out, "     Config is saved. Run `contenox doctor` to check connectivity.")
		fmt.Fprintln(out, "")
		return
	}
	opts.EffectiveDefaultProvider = provider
	if model != "" {
		opts.EffectiveDefaultModel = model
	}

	fmt.Fprintln(out, "")
	fmt.Fprintln(out, "  Checking reachability (no test prompt is sent)...")
	res, err := ComputeReadiness(ctx, db, opts)
	if err != nil {
		fmt.Fprintf(out, "  ⚠  Could not verify setup: %v\n", err)
		fmt.Fprintln(out, "     Config is saved. Run `contenox doctor` to check connectivity.")
		fmt.Fprintln(out, "")
		return
	}
	if res.Ready() {
		fmt.Fprintln(out, "")
		fmt.Fprintln(out, "  ✓ Setup ready — provider reachable and a chat model is available.")
		fmt.Fprintln(out, "  Close this tab and start chatting!")
		fmt.Fprintln(out, "")
		return
	}
	fmt.Fprintln(out, "")
	fmt.Fprintln(out, "  Config saved, but the agent is not ready yet:")
	PrintSetupIssues(out, res)
	fmt.Fprintln(out, "")
	fmt.Fprintln(out, "  Fix the above (or run `contenox doctor`), then you're set.")
	fmt.Fprintln(out, "")
}

func registerSetupBackend(ctx context.Context, db libdb.DBManager, providerType, apiKey, baseURL string) error {
	svc := backendservice.New(db)

	backendURL := strings.TrimSpace(baseURL)
	if backendURL == "" {
		// Account-specific providers (vertex-google) supply baseURL from the wizard.
		switch providerType {
		case "ollama":
			if base, ok := setupcheck.ProbeLocalOllamaAPI(ctx); ok {
				backendURL = base
			} else {
				backendURL = "http://127.0.0.1:11434"
			}
		case "openai":
			backendURL = "https://api.openai.com/v1"
		case "gemini":
			backendURL = "https://generativelanguage.googleapis.com"
		}
	}

	existing, _ := svc.List(ctx, nil, 100)
	for _, b := range existing {
		if !strings.EqualFold(b.Type, providerType) {
			continue
		}
		// Only touch the record when the URL actually changed.
		if backendURL != "" && b.BaseURL != backendURL {
			b.BaseURL = backendURL
			if err := svc.Update(ctx, b); err != nil {
				return fmt.Errorf("update %s backend: %w", providerType, err)
			}
		}
		return nil
	}

	backend := &runtimetypes.Backend{
		ID:      uuid.NewString(),
		Name:    providerType,
		BaseURL: backendURL,
		Type:    providerType,
	}
	if err := svc.Create(ctx, backend); err != nil {
		return fmt.Errorf("register %s backend: %w", providerType, err)
	}

	if apiKey != "" {
		store := runtimetypes.New(db.WithoutTransaction())
		key := runtimestate.ProviderKeyPrefix + providerType
		cfg := runtimestate.ProviderConfig{APIKey: apiKey, Type: providerType}
		data, _ := json.Marshal(cfg)
		_ = store.SetKV(ctx, key, data)
	}
	return nil
}

func promptOllamaModel(out io.Writer, scanner *bufio.Scanner, defaultModel string) string {
	ctx := context.Background()
	if base, ok := setupcheck.ProbeLocalOllamaAPI(ctx); ok {
		fmt.Fprintf(out, "  ✓ Ollama is running at %s\n\n", base)
	} else {
		fmt.Fprintln(out, "")
		fmt.Fprintln(out, "  ⚠ Ollama is not reachable. Make sure 'ollama serve' is running,")
		fmt.Fprintln(out, "    then pull a model: ollama pull qwen2.5:7b")
		fmt.Fprintln(out, "")
	}
	return promptLine(out, scanner, fmt.Sprintf("  Model [%s]", defaultModel), defaultModel)
}

// promptEOF signals the input stream ended before an answer was given,
// distinct from -1 (an intentional "q" quit).
const promptEOF = -2

func promptChoiceOrQuit(out io.Writer, scanner *bufio.Scanner, label string, max int, allowQuit bool) int {
	for {
		if allowQuit {
			fmt.Fprintf(out, "%s (1-%d, q to quit): ", label, max)
		} else {
			fmt.Fprintf(out, "%s (1-%d): ", label, max)
		}
		if !scanner.Scan() {
			return promptEOF
		}
		text := strings.TrimSpace(scanner.Text())
		if allowQuit && strings.EqualFold(text, "q") {
			return -1
		}
		n, err := strconv.Atoi(text)
		if err == nil && n >= 1 && n <= max {
			return n - 1
		}
		if allowQuit {
			fmt.Fprintf(out, "  Please enter a number between 1 and %d, or 'q' to keep current config.\n", max)
		} else {
			fmt.Fprintf(out, "  Please enter a number between 1 and %d.\n", max)
		}
	}
}

func promptLine(out io.Writer, scanner *bufio.Scanner, label, defaultVal string) string {
	fmt.Fprintf(out, "%s: ", label)
	if !scanner.Scan() {
		return defaultVal
	}
	v := strings.TrimSpace(scanner.Text())
	if v == "" {
		return defaultVal
	}
	return v
}

func promptSecret(out io.Writer, scanner *bufio.Scanner, label string) string {
	fd := int(os.Stdin.Fd())
	if term.IsTerminal(fd) {
		fmt.Fprintf(out, "%s: ", label)
		bytes, err := term.ReadPassword(fd)
		fmt.Fprintln(out)
		if err != nil {
			return ""
		}
		return strings.TrimSpace(string(bytes))
	}
	return promptLine(out, scanner, label, "")
}

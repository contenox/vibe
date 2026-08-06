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
	"time"

	libdb "github.com/contenox/contenox/internal/libdbexec"
	"github.com/contenox/contenox/internal/models/backendservice"
	"github.com/contenox/contenox/internal/models/runtimestate"
	"github.com/contenox/contenox/internal/services/clikv"
	"github.com/contenox/contenox/internal/services/onboarding"
	"github.com/contenox/contenox/internal/services/setupcheck"
	"github.com/contenox/contenox/internal/store/runtimetypes"
	"github.com/contenox/contenox/libtracker"
	"github.com/google/uuid"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

var setupCmd = &cobra.Command{
	Use:   "setup",
	Short: "Interactive wizard to configure your LLM provider and model.",
	Long: `Run the setup wizard to pick an LLM provider (Ollama local or Cloud,
OpenAI, Anthropic, Gemini, Vertex AI, AWS Bedrock, or self-hosted vLLM), enter
credentials, and set defaults. This is the same wizard that runs inside IDE
terminals via ACP.

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
	// fixedBaseURL is registered verbatim without prompting; it also
	// distinguishes hosted variants sharing a key with a local entry
	// (Ollama Cloud vs. the local Ollama daemon).
	fixedBaseURL string
}

// setupProviders is the terminal wizard's provider menu; it duplicates
// providerservice.providerDefaultsByType (minus the internal-only "local"
// type), so keep the two in sync.
var setupProviders = []setupProvider{
	{key: "ollama", label: "Ollama (local daemon)", defaultModel: "qwen3:8b", needsAPIKey: false},
	{key: "ollama", label: "Ollama Cloud", defaultModel: "gpt-oss:20b", envKey: "OLLAMA_API_KEY", needsAPIKey: true, fixedBaseURL: "https://ollama.com/api"},
	{key: "openai", label: "OpenAI", defaultModel: "gpt-5-mini", envKey: "OPENAI_API_KEY", needsAPIKey: true},
	{key: "anthropic", label: "Anthropic", defaultModel: "claude-sonnet-4-5", envKey: "ANTHROPIC_API_KEY", needsAPIKey: true},
	{key: "gemini", label: "Google Gemini", defaultModel: "gemini-flash-latest", envKey: "GEMINI_API_KEY", needsAPIKey: true},
	{key: "vertex-google", label: "Google Vertex AI (Gemini via gcloud ADC)", defaultModel: "gemini-3.6-flash", needsAPIKey: false, needsBaseURL: true, baseURLHint: "https://aiplatform.googleapis.com/v1/projects/YOUR_PROJECT/locations/global"},
	{key: "bedrock", label: "AWS Bedrock", defaultModel: "us.anthropic.claude-3-5-sonnet-20241022-v2:0", needsAPIKey: false, needsBaseURL: true, baseURLHint: "https://bedrock-runtime.eu-central-1.amazonaws.com"},
	{key: "vllm", label: "vLLM (self-hosted)", needsAPIKey: false, needsBaseURL: true, baseURLHint: "http://localhost:8000"},
}

// setupProviderKeys returns the wizard's provider keys, deduplicated in menu
// order (the local Ollama daemon and Ollama Cloud entries share key "ollama").
func setupProviderKeys() []string {
	keys := make([]string, 0, len(setupProviders))
	seen := make(map[string]bool, len(setupProviders))
	for _, sp := range setupProviders {
		if seen[sp.key] {
			continue
		}
		seen[sp.key] = true
		keys = append(keys, sp.key)
	}
	return keys
}

// errSetupNoInput is returned when the interactive provider prompt hits EOF, so
// setup exits non-zero instead of silently committing a default.
var errSetupNoInput = errors.New("setup: no input received (interactive); nothing changed")

// ollamaModelListTimeout bounds the wizard's enumeration of a local daemon's
// models (probe plus one /api/show per model).
const ollamaModelListTimeout = 10 * time.Second

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
	} else {
		fmt.Fprintln(out, "    q. Quit without changes")
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
		if alreadyConfigured {
			fmt.Fprintln(out, "  ✓ Keeping current configuration.")
		} else {
			fmt.Fprintln(out, "  Quit — no changes made.")
		}
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

	baseURL := sp.fixedBaseURL
	if sp.needsBaseURL {
		if sp.key == "vertex-google" {
			fmt.Fprintln(out, "  Vertex AI authenticates with Google Cloud Application Default Credentials.")
			fmt.Fprintln(out, "  If you have not already, run:")
			fmt.Fprintln(out, "    gcloud auth application-default login --project YOUR_PROJECT")
			fmt.Fprintln(out, "")
		}
		if sp.key == "bedrock" {
			fmt.Fprintln(out, "  Bedrock authenticates via the standard AWS credential chain (env vars,")
			fmt.Fprintln(out, "  shared profile, or IAM role) — no API key is stored here.")
			fmt.Fprintln(out, "  The AWS region is part of the endpoint URL.")
			fmt.Fprintln(out, "")
		}
		if sp.key == "vertex-google" {
			fmt.Fprintf(out, "  Enter the %s endpoint URL (with your project and location):\n", sp.label)
		} else {
			fmt.Fprintf(out, "  Enter the %s endpoint URL:\n", sp.label)
		}
		if sp.baseURLHint != "" {
			fmt.Fprintf(out, "    e.g. %s\n", sp.baseURLHint)
		}
		if sp.key == "vertex-google" {
			fmt.Fprintln(out, "    or regional (data residency): https://{REGION}-aiplatform.googleapis.com/v1/projects/YOUR_PROJECT/locations/{REGION}")
			fmt.Fprintln(out, "    Model availability differs per endpoint — check with `contenox model list` afterwards.")
		}
		baseURL = promptLine(out, scanner, "  URL", "")
		if baseURL == "" {
			return fmt.Errorf("an endpoint URL is required for %s", sp.label)
		}
		fmt.Fprintln(out, "")
	}

	model := sp.defaultModel
	switch {
	case sp.key == "ollama" && sp.fixedBaseURL == "":
		model = promptOllamaModel(out, scanner, model)
	case model == "":
		// No fixed default (vllm): the server decides what it serves.
		fmt.Fprintln(out, "  Enter the model id your server exposes (verify with `contenox model list` after setup):")
		model = promptLine(out, scanner, "  Model", "")
	default:
		if sp.key == "bedrock" {
			fmt.Fprintln(out, "  The model must be enabled for your AWS account (Bedrock console → Model access).")
			fmt.Fprintln(out, "  List enabled ids with `contenox model list` after setup.")
		}
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
	_ = clikv.WriteConfig(ctx, store, "", "default-provider", sp.key)
	if model != "" {
		_ = clikv.WriteConfig(ctx, store, "", "default-model", model)
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
		printSetupNextCommand(out, stdoutIsTerminal())
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

// printSetupNextCommand names the command that follows a successful wizard run.
// The terminal UI is the flagship surface but needs a terminal, so it is only
// offered when the wizard itself is writing to one; a piped or redirected run
// gets the one-shot form instead. Same register as the failure branch:
// a command the operator can type, not a gesture.
func printSetupNextCommand(out io.Writer, tty bool) {
	if tty {
		fmt.Fprintln(out, "  Next: run `contenox new` for the terminal UI, or `contenox \"your first prompt\"`.")
		return
	}
	fmt.Fprintln(out, "  Next: run `contenox \"your first prompt\"` (the terminal UI, `contenox new`, needs a terminal).")
}

// stdoutIsTerminal reports whether this process's stdout is a terminal. It
// reads os.Stdout rather than a command's writer because cobra's OutOrStdout
// is a buffer under test and a pipe under redirection — the question here is
// about the process, not the writer.
func stdoutIsTerminal() bool {
	return term.IsTerminal(int(os.Stdout.Fd()))
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
		case "anthropic":
			backendURL = "https://api.anthropic.com"
		case "gemini":
			backendURL = "https://generativelanguage.googleapis.com"
		}
	}

	// Key stored before the update/create split: re-running setup must
	// refresh credentials for an already-registered backend too.
	if apiKey != "" {
		store := runtimetypes.New(db.WithoutTransaction())
		key := runtimestate.ProviderKeyPrefix + providerType
		cfg := runtimestate.ProviderConfig{APIKey: apiKey, Type: providerType}
		data, _ := json.Marshal(cfg)
		_ = store.SetKV(ctx, key, data)
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
	return nil
}

// promptOllamaModel asks which local model to make the default. The daemon
// already knows what it serves, so a successful probe answers with a numbered
// menu of the pulled chat-capable models (preselectOllamaModel decides the
// entry Enter accepts) instead of asking the operator to retype a name. A
// probe that fails, or one reporting no chat-capable model, falls back to the
// free-text prompt.
func promptOllamaModel(out io.Writer, scanner *bufio.Scanner, defaultModel string) string {
	// Bounded well under modelrepo.DefaultCallTimeout: enumerating a local
	// daemon happens between two keystrokes of an interactive wizard, and a
	// slow /api/show must degrade to the free-text prompt, not stall it.
	ctx, cancel := context.WithTimeout(context.Background(), ollamaModelListTimeout)
	defer cancel()
	probe := onboarding.ProbeOllamaModels(ctx)
	if !probe.Reachable {
		fmt.Fprintln(out, "")
		fmt.Fprintln(out, "  ⚠ Ollama is not reachable. Make sure 'ollama serve' is running,")
		fmt.Fprintf(out, "    then pull a model: ollama pull %s\n", setupcheck.DefaultOllamaSuggestModel)
		fmt.Fprintln(out, "")
		return promptLine(out, scanner, fmt.Sprintf("  Model [%s]", defaultModel), defaultModel)
	}
	fmt.Fprintf(out, "  ✓ Ollama is running at %s\n\n", probe.BaseURL)

	models := probe.ChatModels()
	if len(models) == 0 {
		// Covers both "nothing pulled" and "listing degraded" — the probe does
		// not distinguish them, so the message must be true of either.
		fmt.Fprintln(out, "  No chat-capable model reported by this daemon.")
		fmt.Fprintf(out, "    Pull one with: ollama pull %s — or enter a model id below.\n", setupcheck.DefaultOllamaSuggestModel)
		fmt.Fprintln(out, "")
		return promptLine(out, scanner, fmt.Sprintf("  Model [%s]", defaultModel), defaultModel)
	}
	return promptOllamaModelMenu(out, scanner, models, preselectOllamaModel(models, defaultModel))
}

// preselectOllamaModel picks the menu entry Enter accepts: the suggested
// default when the daemon actually serves it, otherwise the first pulled
// chat model — never a name that is not on the menu.
func preselectOllamaModel(models []string, defaultModel string) string {
	for _, m := range models {
		if m == defaultModel {
			return m
		}
	}
	return models[0]
}

// promptOllamaModelMenu renders the pulled chat models as a numbered list with
// preselected marked, and resolves the answer via resolveOllamaModelChoice.
func promptOllamaModelMenu(out io.Writer, scanner *bufio.Scanner, models []string, preselected string) string {
	fmt.Fprintln(out, "  Chat models pulled on this daemon:")
	fmt.Fprintln(out, "")
	preselectedIdx := 1
	for i, m := range models {
		marker := ""
		if m == preselected {
			marker = "  (default)"
			preselectedIdx = i + 1
		}
		fmt.Fprintf(out, "    %d. %s%s\n", i+1, m, marker)
	}
	fmt.Fprintln(out, "")
	for {
		answer := promptLine(out, scanner, fmt.Sprintf("  Model [%d]", preselectedIdx), "")
		model, ok := resolveOllamaModelChoice(answer, models, preselected)
		if ok {
			return model
		}
		fmt.Fprintf(out, "  Please enter a number between 1 and %d, or a model id.\n", len(models))
	}
}

// resolveOllamaModelChoice maps a menu answer to a model id: an empty answer
// (also EOF) keeps preselected, an in-range 1-based number picks that entry,
// and any non-numeric answer is taken literally so a model the probe could not
// classify — or one pulled after the probe — is still reachable. An
// out-of-range number resolves to no model (ok false) rather than silently
// committing the preselected one.
func resolveOllamaModelChoice(answer string, models []string, preselected string) (string, bool) {
	answer = strings.TrimSpace(answer)
	if answer == "" {
		return preselected, true
	}
	if n, err := strconv.Atoi(answer); err == nil {
		if n >= 1 && n <= len(models) {
			return models[n-1], true
		}
		return "", false
	}
	return answer, true
}

// promptEOF signals the input stream ended before an answer was given,
// distinct from -1 (an intentional "q" quit).
const promptEOF = -2

// promptChoiceOrQuit reads a 1-based menu choice, returning its zero-based
// index, -1 for an intentional "q" (always accepted), or promptEOF when input
// ends first. keepCurrent only selects the quit hint's wording.
func promptChoiceOrQuit(out io.Writer, scanner *bufio.Scanner, label string, max int, keepCurrent bool) int {
	for {
		fmt.Fprintf(out, "%s (1-%d, q to quit): ", label, max)
		if !scanner.Scan() {
			return promptEOF
		}
		text := strings.TrimSpace(scanner.Text())
		if strings.EqualFold(text, "q") {
			return -1
		}
		n, err := strconv.Atoi(text)
		if err == nil && n >= 1 && n <= max {
			return n - 1
		}
		if keepCurrent {
			fmt.Fprintf(out, "  Please enter a number between 1 and %d, or 'q' to keep current config.\n", max)
		} else {
			fmt.Fprintf(out, "  Please enter a number between 1 and %d, or 'q' to quit without changes.\n", max)
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

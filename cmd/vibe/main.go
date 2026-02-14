// Contenox Vibe: run task chains locally with SQLite, in-memory bus, and estimate tokenizer.
// No Postgres, no NATS, no tokenizer service. Use for dev/admin shadow orchestration.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/contenox/vibe/backendservice"
	"github.com/contenox/vibe/execservice"
	"github.com/contenox/vibe/internal/hooks"
	"github.com/contenox/vibe/jseval"
	"github.com/contenox/vibe/internal/llmrepo"
	"github.com/contenox/vibe/internal/ollamatokenizer"
	"github.com/contenox/vibe/internal/runtimestate"
	libbus "github.com/contenox/vibe/libbus"
	libdb "github.com/contenox/vibe/libdbexec"
	"github.com/contenox/vibe/libtracker"
	"github.com/contenox/vibe/localhooks"
	"github.com/contenox/vibe/runtimetypes"
	"github.com/contenox/vibe/taskengine"
	"github.com/google/uuid"
	"gopkg.in/yaml.v3"
)

const localTenantID = "00000000-0000-0000-0000-000000000001"

const (
	defaultOllama  = "http://127.0.0.1:11434"
	defaultModel   = "phi3:3.8b"
	defaultContext = 2048
	defaultTimeout = 5 * time.Minute
)

// extraModelEntry is one entry under extra_models in config (name + context required; capabilities optional).
type extraModelEntry struct {
	Name      string `yaml:"name"`
	Context   int    `yaml:"context"`
	CanChat   *bool  `yaml:"can_chat"`
	CanPrompt *bool  `yaml:"can_prompt"`
	CanEmbed  *bool  `yaml:"can_embed"`
}

// localConfig holds optional values from .contenox/config.yaml (flags override).
type localConfig struct {
	DefaultChain             string            `yaml:"default_chain"`
	DB                       string            `yaml:"db"`
	Ollama                   string            `yaml:"ollama"`
	Model                    string            `yaml:"model"`
	Context                  *int              `yaml:"context"`
	NoDeleteModels           *bool             `yaml:"no_delete_models"`
	EnableLocalExec          *bool             `yaml:"enable_local_exec"`
	LocalExecAllowedDir      string            `yaml:"local_exec_allowed_dir"`
	LocalExecAllowedCommands string            `yaml:"local_exec_allowed_commands"`
	LocalExecDeniedCommands  []string          `yaml:"local_exec_denied_commands"`
	ExtraModels              []extraModelEntry `yaml:"extra_models"`
	Tracing                  *bool             `yaml:"tracing"`
	Steps                    *bool             `yaml:"steps"`
	Raw                      *bool             `yaml:"raw"`
	TemplateVarsFromEnv      []string          `yaml:"template_vars_from_env"`
}

// loadLocalConfig tries ./.contenox/config.yaml then ~/.contenox/config.yaml.
// Returns (config, pathToConfigFile, nil). If neither file exists, returns (empty, "", nil).
// The config file path is used to resolve default_chain relative to that .contenox directory.
func loadLocalConfig() (localConfig, string, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return localConfig{}, "", err
	}
	try := []string{
		filepath.Join(cwd, ".contenox", "config.yaml"),
	}
	if home, err := os.UserHomeDir(); err == nil {
		try = append(try, filepath.Join(home, ".contenox", "config.yaml"))
	}
	for _, p := range try {
		data, err := os.ReadFile(p)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return localConfig{}, "", err
		}
		var cfg localConfig
		if err := yaml.Unmarshal(data, &cfg); err != nil {
			return localConfig{}, "", fmt.Errorf("%s: %w", p, err)
		}
		return cfg, p, nil
	}
	return localConfig{}, "", nil
}

// splitAndTrim splits s by sep and trims each element.
func splitAndTrim(s, sep string) []string {
	var out []string
	for _, p := range strings.Split(s, sep) {
		if t := strings.TrimSpace(p); t != "" {
			out = append(out, t)
		}
	}
	return out
}

// lastAssistantContentFromHistory returns the content of the last assistant message with non-empty content.
// For chat_history output this is the "true" reply to the user.
func lastAssistantContentFromHistory(chat taskengine.ChatHistory) string {
	for i := len(chat.Messages) - 1; i >= 0; i-- {
		m := chat.Messages[i]
		if m.Role == "assistant" && m.Content != "" {
			return m.Content
		}
	}
	return ""
}

// printRelevantOutput prints only the relevant part of the result based on output type, unless raw is true.
// - raw: full output (pretty-printed JSON or string as-is).
// - chat_history: last assistant message content only.
// - string: the string.
// - other: pretty-printed JSON.
func printRelevantOutput(output any, outputType taskengine.DataType, raw bool) {
	if raw {
		printOutput(output)
		return
	}
	switch outputType {
	case taskengine.DataTypeChatHistory:
		if ch, ok := output.(taskengine.ChatHistory); ok {
			if content := lastAssistantContentFromHistory(ch); content != "" {
				fmt.Println(content)
				return
			}
		}
	case taskengine.DataTypeString:
		if s, ok := output.(string); ok {
			fmt.Println(s)
			return
		}
	}
	printOutput(output)
}

// printOutput prints output in a human-friendly way:
// - if it's a string, prints it directly
// - otherwise pretty-prints as JSON
func printOutput(output any) {
	switch v := output.(type) {
	case string:
		fmt.Println(v)
	case []byte:
		fmt.Println(string(v))
	default:
		b, err := json.MarshalIndent(output, "", "  ")
		if err != nil {
			// fallback to default formatting
			fmt.Println(output)
			return
		}
		fmt.Println(string(b))
	}
}

// formatDuration formats a duration for step output (e.g. "1.70s", "53ms").
func formatDuration(d time.Duration) string {
	if d < time.Millisecond {
		return fmt.Sprintf("%dµs", d.Microseconds())
	}
	if d < time.Second {
		return fmt.Sprintf("%.0fms", d.Seconds()*1000)
	}
	return fmt.Sprintf("%.2fs", d.Seconds())
}

// isFlagPassed returns true if the flag with the given name was explicitly set on the command line.
func isFlagPassed(name string) bool {
	found := false
	flag.Visit(func(f *flag.Flag) {
		if f.Name == name {
			found = true
		}
	})
	return found
}

func main() {
	// Define flags
	dbPath := flag.String("db", "", "SQLite database path (default: .contenox/local.db)")
	baseURL := flag.String("ollama", defaultOllama, "Ollama base URL")
	model := flag.String("model", defaultModel, "Model name (task/chat/embed)")
	contextLen := flag.Int("context", defaultContext, "Context length")
	noDeleteModels := flag.Bool("no-delete-models", true, "Do not delete Ollama models that are not declared (default true for vibe; allows pre-pulled models)")
	chainPath := flag.String("chain", "", "Path to task chain JSON file (required)")
	input := flag.String("input", "", "Input string for the chain (default: read from stdin if piped, else empty)")
	enableLocalExec := flag.Bool("enable-local-exec", false, "Enable the local_exec hook (runs commands on this host; use only in trusted environments)")
	localExecAllowedDir := flag.String("local-exec-allowed-dir", "", "If set, local_exec may only run scripts/binaries under this directory")
	localExecAllowedCommands := flag.String("local-exec-allowed-commands", "", "Comma-separated list of allowed executable paths/names for local_exec (if set)")
	localExecDeniedCommands := flag.String("local-exec-denied-commands", "", "Comma-separated list of denied executable basenames/paths for local_exec (checked before allowlist)")
	timeout := flag.Duration("timeout", defaultTimeout, "Maximum execution time (e.g., 5m, 1h)")
	tracing := flag.Bool("tracing", false, "Enable operation telemetry (operation started/completed, state changes) on stderr")
	steps := flag.Bool("steps", false, "Print execution steps (task list with handler and duration) after the result")
	raw := flag.Bool("raw", false, "Print full output (e.g. entire chat JSON); default is to print only the relevant part (e.g. last assistant reply for chat)")
	flag.Parse()

	// Load optional config; apply as defaults where flag was not explicitly set.
	cfg, configPath, err := loadLocalConfig()
	if err != nil {
		slog.Error("Failed to load config", "error", err)
		os.Exit(1)
	}

	// Determine .contenox directory: where config was found, or cwd/.contenox
	var contenoxDir string
	if configPath != "" {
		contenoxDir = filepath.Dir(configPath)
	} else {
		cwd, _ := os.Getwd()
		contenoxDir = filepath.Join(cwd, ".contenox")
	}

	// Merge config values, but only if the corresponding flag was NOT explicitly set.
	effectiveDB := *dbPath
	if effectiveDB == "" && !isFlagPassed("db") && cfg.DB != "" {
		effectiveDB = cfg.DB
	}
	if effectiveDB == "" {
		effectiveDB = filepath.Join(contenoxDir, "local.db")
	}

	effectiveOllama := *baseURL
	if effectiveOllama == defaultOllama && !isFlagPassed("ollama") && cfg.Ollama != "" {
		effectiveOllama = cfg.Ollama
	}

	effectiveModel := *model
	if effectiveModel == defaultModel && !isFlagPassed("model") && cfg.Model != "" {
		effectiveModel = cfg.Model
	}

	effectiveContext := *contextLen
	if effectiveContext == defaultContext && !isFlagPassed("context") && cfg.Context != nil {
		effectiveContext = *cfg.Context
	}

	effectiveNoDeleteModels := *noDeleteModels
	if effectiveNoDeleteModels == true && !isFlagPassed("no-delete-models") && cfg.NoDeleteModels != nil {
		effectiveNoDeleteModels = *cfg.NoDeleteModels
	}

	effectiveChain := *chainPath
	if effectiveChain == "" && !isFlagPassed("chain") && cfg.DefaultChain != "" {
		// default_chain is relative to the .contenox directory (chains live inside .contenox).
		effectiveChain = filepath.Join(contenoxDir, cfg.DefaultChain)
	}
	if effectiveChain == "" && !isFlagPassed("chain") {
		wellKnown := filepath.Join(contenoxDir, "default-chain.json")
		if _, err := os.Stat(wellKnown); err == nil {
			effectiveChain = wellKnown
		}
	}
	if effectiveChain == "" {
		slog.Error("No chain file specified", "hint", "use -chain <path>, or set default_chain in .contenox/config.yaml, or add .contenox/default-chain.json")
		os.Exit(1)
	}

	effectiveEnableLocalExec := *enableLocalExec
	if !effectiveEnableLocalExec && !isFlagPassed("enable-local-exec") && cfg.EnableLocalExec != nil {
		effectiveEnableLocalExec = *cfg.EnableLocalExec
	}

	effectiveLocalExecAllowedDir := *localExecAllowedDir
	if effectiveLocalExecAllowedDir == "" && !isFlagPassed("local-exec-allowed-dir") && cfg.LocalExecAllowedDir != "" {
		effectiveLocalExecAllowedDir = cfg.LocalExecAllowedDir
	}

	effectiveLocalExecAllowedCommands := *localExecAllowedCommands
	if effectiveLocalExecAllowedCommands == "" && !isFlagPassed("local-exec-allowed-commands") && cfg.LocalExecAllowedCommands != "" {
		effectiveLocalExecAllowedCommands = cfg.LocalExecAllowedCommands
	}

	var effectiveLocalExecDeniedCommands []string
	if isFlagPassed("local-exec-denied-commands") {
		effectiveLocalExecDeniedCommands = splitAndTrim(*localExecDeniedCommands, ",")
	} else if len(cfg.LocalExecDeniedCommands) > 0 {
		effectiveLocalExecDeniedCommands = cfg.LocalExecDeniedCommands
	}

	effectiveTracing := *tracing
	if !effectiveTracing && !isFlagPassed("tracing") && cfg.Tracing != nil {
		effectiveTracing = *cfg.Tracing
	}

	effectiveSteps := *steps
	if !effectiveSteps && !isFlagPassed("steps") && cfg.Steps != nil {
		effectiveSteps = *cfg.Steps
	}

	effectiveRaw := *raw
	if !effectiveRaw && !isFlagPassed("raw") && cfg.Raw != nil {
		effectiveRaw = *cfg.Raw
	}

	// Validate allowed commands: if a directory is set, all allowed commands must be within that directory (or be simple names).
	// This is a basic sanity check; the hook itself will enforce stricter rules.
	if effectiveEnableLocalExec && effectiveLocalExecAllowedDir != "" && effectiveLocalExecAllowedCommands != "" {
		allowedDir, err := filepath.Abs(effectiveLocalExecAllowedDir)
		if err != nil {
			slog.Error("Invalid allowed directory", "error", err)
			os.Exit(1)
		}
		commands := splitAndTrim(effectiveLocalExecAllowedCommands, ",")
		for _, cmd := range commands {
			// If it's an absolute path, it must be under allowedDir.
			if filepath.IsAbs(cmd) {
				if !strings.HasPrefix(cmd, allowedDir) {
					slog.Error("Command path not inside allowed directory", "command", cmd, "allowed_dir", allowedDir)
					os.Exit(1)
				}
			}
			// Relative paths are interpreted relative to allowedDir; we don't resolve here, the hook will.
		}
	}

	// Setup context with timeout and signal cancellation.
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		slog.Warn("Received interrupt, shutting down...")
		cancel()
	}()

	// Add request ID to context for tracing.
	ctx = context.WithValue(ctx, libtracker.ContextKeyRequestID, uuid.New().String())

	// ------------------------------------------------------------------------
	// 1. SQLite database
	// ------------------------------------------------------------------------
	dbPathAbs, err := filepath.Abs(effectiveDB)
	if err != nil {
		slog.Error("Invalid database path", "error", err)
		os.Exit(1)
	}
	// Ensure parent directory exists
	if err := os.MkdirAll(filepath.Dir(dbPathAbs), 0755); err != nil {
		slog.Error("Cannot create database directory", "error", err)
		os.Exit(1)
	}
	db, err := libdb.NewSQLiteDBManager(ctx, dbPathAbs, runtimetypes.SchemaSQLite)
	if err != nil {
		slog.Error("Failed to open SQLite database", "error", err)
		os.Exit(1)
	}
	defer func() {
		if err := db.Close(); err != nil {
			slog.Error("Error closing database", "error", err)
		}
	}()

	// ------------------------------------------------------------------------
	// 2. In-memory bus
	// ------------------------------------------------------------------------
	bus := libbus.NewInMem()
	defer bus.Close()

	// ------------------------------------------------------------------------
	// 3. Runtime state
	// ------------------------------------------------------------------------
	stateOpts := []runtimestate.Option{}
	if effectiveNoDeleteModels {
		stateOpts = append(stateOpts, runtimestate.WithSkipDeleteUndeclaredModels())
	}
	state, err := runtimestate.New(ctx, db, bus, stateOpts...)
	if err != nil {
		slog.Error("Failed to create runtime state", "error", err)
		os.Exit(1)
	}

	// ------------------------------------------------------------------------
	// 4. Initialize embed/task/chat groups
	// ------------------------------------------------------------------------
	config := &runtimestate.Config{
		TenantID:   localTenantID,
		EmbedModel: effectiveModel,
		TaskModel:  effectiveModel,
		ChatModel:  effectiveModel,
	}
	if err := runtimestate.InitEmbeder(ctx, config, db, effectiveContext, state); err != nil {
		slog.Error("Failed to init embedder", "error", err)
		os.Exit(1)
	}
	if err := runtimestate.InitPromptExec(ctx, config, db, state, effectiveContext); err != nil {
		slog.Error("Failed to init prompt executor", "error", err)
		os.Exit(1)
	}
	if err := runtimestate.InitChatExec(ctx, config, db, state, effectiveContext); err != nil {
		slog.Error("Failed to init chat executor", "error", err)
		os.Exit(1)
	}

	// ------------------------------------------------------------------------
	// 4b. Ensure extra models from config (so they get context/capabilities during backend sync)
	// ------------------------------------------------------------------------
	if len(cfg.ExtraModels) > 0 {
		specs := make([]runtimestate.ExtraModelSpec, 0, len(cfg.ExtraModels))
		for _, e := range cfg.ExtraModels {
			if e.Context <= 0 {
				continue
			}
			canChat := true
			if e.CanChat != nil {
				canChat = *e.CanChat
			}
			canPrompt := true
			if e.CanPrompt != nil {
				canPrompt = *e.CanPrompt
			}
			canEmbed := false
			if e.CanEmbed != nil {
				canEmbed = *e.CanEmbed
			}
			specs = append(specs, runtimestate.ExtraModelSpec{
				Name:          e.Name,
				ContextLength: e.Context,
				CanChat:       canChat,
				CanPrompt:     canPrompt,
				CanEmbed:      canEmbed,
			})
		}
		if len(specs) > 0 {
			if err := runtimestate.EnsureModels(ctx, db, localTenantID, specs); err != nil {
				slog.Error("Failed to ensure extra models", "error", err)
				os.Exit(1)
			}
		}
	}

	// ------------------------------------------------------------------------
	// 5. Get or create one Ollama backend
	// ------------------------------------------------------------------------
	backendSvc := backendservice.New(db)
	backends, err := backendSvc.List(ctx, nil, 50)
	if err != nil {
		slog.Error("Failed to list backends", "error", err)
		os.Exit(1)
	}
	var backend *runtimetypes.Backend
	for _, b := range backends {
		if b.BaseURL == effectiveOllama {
			backend = b
			break
		}
	}
	if backend == nil {
		backend = &runtimetypes.Backend{
			ID:      uuid.NewString(),
			Name:    "vibe-ollama",
			BaseURL: effectiveOllama,
			Type:    "ollama",
		}
		if err := backendSvc.Create(ctx, backend); err != nil {
			slog.Error("Failed to create backend", "error", err)
			os.Exit(1)
		}
	}

	// ------------------------------------------------------------------------
	// 6. Run one backend cycle to sync models
	// ------------------------------------------------------------------------
	if effectiveTracing {
		slog.Info("Running backend cycle to sync models...")
	}
	if err := state.RunBackendCycle(ctx); err != nil {
		slog.Warn("Backend cycle encountered errors", "error", err)
	}
	// Log runtime state for debugging (only when tracing)
	rt := state.Get(ctx)
	anyReachable := false
	for id, bs := range rt {
		if bs.Error != "" {
			if effectiveTracing {
				slog.Warn("Backend unreachable", "id", id, "url", bs.Backend.BaseURL, "error", bs.Error)
			}
		} else {
			anyReachable = true
			if effectiveTracing {
				slog.Info("Backend reachable", "id", id, "url", bs.Backend.BaseURL, "models", len(bs.PulledModels))
				for _, m := range bs.PulledModels {
					slog.Debug("Pulled model", "model", m.Model)
				}
			}
		}
	}
	if !anyReachable && effectiveTracing {
		slog.Warn("No reachable backends – subsequent model operations may fail")
	}

	// ------------------------------------------------------------------------
	// 7. Tokenizer and model manager
	// ------------------------------------------------------------------------
	tokenizer := ollamatokenizer.NewEstimateTokenizer()
	var tracker libtracker.ActivityTracker
	if effectiveTracing {
		tracker = libtracker.NewLogActivityTracker(slog.Default())
	} else {
		tracker = libtracker.NoopTracker{}
	}
	repo, err := llmrepo.NewModelManager(state, tokenizer, llmrepo.ModelManagerConfig{
		DefaultPromptModel:   llmrepo.ModelConfig{Name: effectiveModel, Provider: "ollama"},
		DefaultEmbeddingModel: llmrepo.ModelConfig{Name: effectiveModel, Provider: "ollama"},
		DefaultChatModel:      llmrepo.ModelConfig{Name: effectiveModel, Provider: "ollama"},
	}, tracker)
	if err != nil {
		slog.Error("Failed to create model manager", "error", err)
		os.Exit(1)
	}

	// ------------------------------------------------------------------------
	// 8. Local hooks
	// ------------------------------------------------------------------------
	jsEnv := jseval.NewEnv(tracker, jseval.BuiltinHandlers{})
	localHooks := map[string]taskengine.HookRepo{
		"echo":       localhooks.NewEchoHook(),
		"print":      localhooks.NewPrint(tracker),
		"webhook":    localhooks.NewWebCaller(),
		"js_sandbox": localhooks.NewJSSandboxHook(jsEnv, tracker),
	}
	if sshHook, err := localhooks.NewSSHHook(); err != nil {
		slog.Debug("SSH hook not registered (e.g. no known_hosts)", "error", err)
	} else {
		localHooks["ssh"] = sshHook
	}
	if effectiveEnableLocalExec {
		opts := []localhooks.LocalExecOption{}
		if effectiveLocalExecAllowedDir != "" {
			opts = append(opts, localhooks.WithLocalExecAllowedDir(effectiveLocalExecAllowedDir))
		}
		if effectiveLocalExecAllowedCommands != "" {
			commands := splitAndTrim(effectiveLocalExecAllowedCommands, ",")
			if len(commands) > 0 {
				opts = append(opts, localhooks.WithLocalExecAllowedCommands(commands))
			}
		}
		if len(effectiveLocalExecDeniedCommands) > 0 {
			opts = append(opts, localhooks.WithLocalExecDeniedCommands(effectiveLocalExecDeniedCommands))
		}
		localHooks["local_exec"] = localhooks.NewLocalExecHook(opts...)
	}
	hookRepo := hooks.NewPersistentRepo(localHooks, db, http.DefaultClient)

	// ------------------------------------------------------------------------
	// 9. Task engine
	// ------------------------------------------------------------------------
	exec, err := taskengine.NewExec(ctx, repo, hookRepo, tracker)
	if err != nil {
		slog.Error("Failed to create task executor", "error", err)
		os.Exit(1)
	}
	envExec, err := taskengine.NewEnv(ctx, tracker, exec, taskengine.NewSimpleInspector(), hookRepo)
	if err != nil {
		slog.Error("Failed to create environment executor", "error", err)
		os.Exit(1)
	}
	envExec, err = taskengine.NewMacroEnv(envExec, hookRepo)
	if err != nil {
		slog.Error("Failed to create macro environment", "error", err)
		os.Exit(1)
	}

	taskService := execservice.NewTasksEnv(ctx, envExec, hookRepo)

	// ------------------------------------------------------------------------
	// 10. Load chain from file
	// ------------------------------------------------------------------------
	chainPathAbs, err := filepath.Abs(effectiveChain)
	if err != nil {
		slog.Error("Invalid chain path", "error", err)
		os.Exit(1)
	}
	chainData, err := os.ReadFile(chainPathAbs)
	if err != nil {
		slog.Error("Failed to read chain file", "path", chainPathAbs, "error", err)
		os.Exit(1)
	}
	var chain taskengine.TaskChainDefinition
	if err := json.Unmarshal(chainData, &chain); err != nil {
		slog.Error("Failed to parse chain JSON", "error", err)
		os.Exit(1)
	}

	// Determine input: either from flag or from stdin if piped.
	in := *input
	if in == "" && !isFlagPassed("input") {
		// Check if stdin is not a terminal (i.e., data is being piped)
		stat, _ := os.Stdin.Stat()
		if (stat.Mode() & os.ModeCharDevice) == 0 {
			bytes, err := io.ReadAll(os.Stdin)
			if err != nil {
				slog.Error("Failed to read from stdin", "error", err)
				os.Exit(1)
			}
			in = string(bytes)
		}
	}
	if in == "" {
		slog.Error("No input for chain", "hint", "pass -input \"your prompt\" or pipe input (e.g. echo 'hello' | bin/contenox-vibe)")
		os.Exit(1)
	}

	// ------------------------------------------------------------------------
	// 11. Execute chain
	// ------------------------------------------------------------------------
	// Template vars for macro expansion ({{var:name}}). Engine never reads env; we only add allowlisted keys here.
	templateVars := map[string]string{
		"model":    effectiveModel,
		"provider": "ollama",
		"chain":    chain.ID,
	}
	for _, key := range cfg.TemplateVarsFromEnv {
		if v := os.Getenv(key); v != "" {
			templateVars[key] = v
		}
	}
	ctx = taskengine.WithTemplateVars(ctx, templateVars)

	// Package the input string as chat_history so chains whose first task is chat_completion (e.g. vibes) receive the expected type.
	chainInput := taskengine.ChatHistory{
		Messages: []taskengine.Message{{Role: "user", Content: in}},
	}
	if effectiveTracing {
		slog.Info("Executing chain", "chain", chainPathAbs)
	} else {
		// Playful thinking message
		fmt.Fprintln(os.Stderr, "Thinking...")
	}
	output, outputType, stateUnits, err := taskService.Execute(ctx, &chain, chainInput, taskengine.DataTypeChatHistory)
	if err != nil {
		slog.Error("Chain execution failed", "error", err)
		os.Exit(1)
	}

	// ------------------------------------------------------------------------
	// 12. Print results in a friendly, vibey format
	// ------------------------------------------------------------------------
	fmt.Println("\n━━━━━━━━━━━━━━━━━━━━")
	printRelevantOutput(output, outputType, effectiveRaw)
	fmt.Println("━━━━━━━━━━━━━━━━━━━━")
	// TODO: ONLY Print on verbose flag (-v) fmt.Printf("🔢 Output Type: %d\n", outputType.String())
	if effectiveSteps && len(stateUnits) > 0 {
		fmt.Println("━━━━━━━━━━━━━━━━━━━━")
		fmt.Println("📋 Steps:")
		for i, u := range stateUnits {
			fmt.Printf("  %d. %s (%s) %s %s\n", i+1, u.TaskID, u.TaskHandler, formatDuration(u.Duration), u.Transition)
		}
		fmt.Println("━━━━━━━━━━━━━━━━━━━━")
	}
}

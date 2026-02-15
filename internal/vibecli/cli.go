// cli.go holds the vibe CLI entrypoint (Main), default constants, flags, and merge logic.
package vibecli

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/contenox/vibe/libtracker"
	"github.com/google/uuid"
)

const localTenantID = "00000000-0000-0000-0000-000000000001"

const (
	defaultOllama  = "http://127.0.0.1:11434"
	defaultModel   = "phi3:3.8b"
	defaultContext = 2048
	defaultTimeout = 5 * time.Minute
)

// Main runs the vibe CLI: init subcommand or full run (flags, config, pipeline).
func Main() {
	if len(os.Args) >= 2 && os.Args[1] == "init" {
		runInit(os.Args[2:])
		return
	}

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

	cfg, configPath, err := loadLocalConfig()
	if err != nil {
		slog.Error("Failed to load config", "error", err)
		os.Exit(1)
	}

	var contenoxDir string
	if configPath != "" {
		contenoxDir = filepath.Dir(configPath)
	} else {
		cwd, _ := os.Getwd()
		contenoxDir = filepath.Join(cwd, ".contenox")
	}

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

	resolvedBackends, effectiveDefaultProvider, effectiveDefaultModel := resolveEffectiveBackends(cfg, effectiveOllama, effectiveModel)
	if isFlagPassed("model") {
		effectiveDefaultModel = effectiveModel
	}

	if effectiveEnableLocalExec && effectiveLocalExecAllowedDir != "" && effectiveLocalExecAllowedCommands != "" {
		allowedDir, err := filepath.Abs(effectiveLocalExecAllowedDir)
		if err != nil {
			slog.Error("Invalid allowed directory", "error", err)
			os.Exit(1)
		}
		commands := splitAndTrim(effectiveLocalExecAllowedCommands, ",")
		for _, cmd := range commands {
			if filepath.IsAbs(cmd) {
				if !strings.HasPrefix(cmd, allowedDir) {
					slog.Error("Command path not inside allowed directory", "command", cmd, "allowed_dir", allowedDir)
					os.Exit(1)
				}
			}
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		slog.Warn("Received interrupt, shutting down...")
		cancel()
	}()

	ctx = context.WithValue(ctx, libtracker.ContextKeyRequestID, uuid.New().String())

	opts := runOpts{
		EffectiveDB:                       effectiveDB,
		EffectiveChain:                    effectiveChain,
		EffectiveDefaultModel:             effectiveDefaultModel,
		EffectiveDefaultProvider:          effectiveDefaultProvider,
		EffectiveContext:                  effectiveContext,
		EffectiveNoDeleteModels:           effectiveNoDeleteModels,
		EffectiveEnableLocalExec:          effectiveEnableLocalExec,
		EffectiveLocalExecAllowedDir:      effectiveLocalExecAllowedDir,
		EffectiveLocalExecAllowedCommands: effectiveLocalExecAllowedCommands,
		EffectiveLocalExecDeniedCommands:  effectiveLocalExecDeniedCommands,
		EffectiveTracing:                  effectiveTracing,
		EffectiveSteps:                    effectiveSteps,
		EffectiveRaw:                      effectiveRaw,
		InputValue:                        *input,
		InputFlagPassed:                   isFlagPassed("input"),
		Cfg:                               cfg,
		ResolvedBackends:                  resolvedBackends,
		ContenoxDir:                       contenoxDir,
	}
	run(ctx, opts)
}

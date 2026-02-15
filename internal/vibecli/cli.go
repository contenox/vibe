// cli.go holds the vibe CLI entrypoint (Main), default constants, flags, and merge logic.
package vibecli

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/contenox/vibe/libtracker"
	"github.com/google/uuid"
	"github.com/spf13/cobra"
)

const localTenantID = "00000000-0000-0000-0000-000000000001"

const (
	defaultOllama  = "http://127.0.0.1:11434"
	defaultModel   = "phi3:3.8b"
	defaultContext = 2048
	defaultTimeout = 5 * time.Minute
)

// reservedSubcommands are first-arg names that must not be treated as run input (Cobra or our subcommands).
var reservedSubcommands = map[string]bool{"init": true, "run": true, "help": true, "completion": true}

// Main runs the vibe CLI: init subcommand or run (default) with optional positional input.
func Main() {
	args := os.Args[1:]
	// Only inject "run" when no reserved subcommand was given (so "vibe completion" and "vibe help" work).
	if len(args) == 0 || !reservedSubcommands[args[0]] {
		rootCmd.SetArgs(append([]string{"run"}, args...))
	}
	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}

var rootCmd = &cobra.Command{
	Use:   "vibe",
	Short: "Run task chains locally (SQLite, in-memory bus).",
	Long: `Run task chains locally with SQLite and an in-memory bus.
Use 'vibe init' to scaffold .contenox/. Use 'vibe <input>' or 'vibe --input <string>' to run a chain.
Options can be set in .contenox/config.yaml; flags override config.`,
	SilenceUsage: true,
}

var runCmd = &cobra.Command{
	Use:   "run",
	Short: "Run a task chain (default when no subcommand is given).",
	Long:  `Run a task chain. You can pass input as positional args (e.g. vibe hi) or via --input.`,
	Args:  cobra.ArbitraryArgs,
	RunE:  runRun,
}

var initCmd = &cobra.Command{
	Use:   "init",
	Short: "Scaffold .contenox/ (config and default chain).",
	Long:  `Create .contenox/config.yaml and .contenox/default-chain.json. Use --force to overwrite existing files.`,
	RunE:  runInitCmd,
}

func init() {
	// Run flags on root so "vibe --input x" and "vibe hi" both work.
	f := rootCmd.PersistentFlags()
	f.String("db", "", "SQLite database path (default: .contenox/local.db)")
	f.String("ollama", defaultOllama, "Ollama base URL")
	f.String("model", defaultModel, "Model name (task/chat/embed)")
	f.Int("context", defaultContext, "Context length")
	f.Bool("no-delete-models", true, "Do not delete Ollama models that are not declared (default true for vibe)")
	f.String("chain", "", "Path to task chain JSON file (required unless default in config)")
	f.String("input", "", "Input for the chain (default: positional args or stdin if piped)")
	f.Bool("enable-local-exec", false, "Enable the local_shell hook (use only in trusted environments)")
	f.String("local-exec-allowed-dir", "", "If set, local_shell may only run scripts/binaries under this directory")
	f.String("local-exec-allowed-commands", "", "Comma-separated list of allowed executable paths/names for local_shell")
	f.String("local-exec-denied-commands", "", "Comma-separated list of denied executable basenames/paths for local_shell")
	f.Duration("timeout", defaultTimeout, "Maximum execution time (e.g., 5m, 1h)")
	f.Bool("tracing", false, "Enable operation telemetry on stderr")
	f.Bool("steps", false, "Print execution steps after the result")
	f.Bool("raw", false, "Print full output (e.g. entire chat JSON)")

	rootCmd.AddCommand(initCmd, runCmd)
	rootCmd.InitDefaultHelpCmd() // so "vibe help" is handled by Cobra, not passed as run input
	initCmd.Flags().BoolP("force", "f", false, "Overwrite existing files")
}

func runInitCmd(cmd *cobra.Command, _ []string) error {
	force, _ := cmd.Flags().GetBool("force")
	RunInit(force)
	return nil
}

func runRun(cmd *cobra.Command, args []string) error {
	// No subcommand and no input: show help and exit 0 (same as "vibe" alone).
	flags := cmd.Root().Flags()
	if len(args) == 0 && !flags.Changed("input") {
		_ = cmd.Root().Usage()
		return nil
	}

	cfg, configPath, err := loadLocalConfig()
	if err != nil {
		slog.Error("Failed to load config", "error", err)
		return err
	}

	var contenoxDir string
	if configPath != "" {
		contenoxDir = filepath.Dir(configPath)
	} else {
		cwd, _ := os.Getwd()
		contenoxDir = filepath.Join(cwd, ".contenox")
	}

	flags = cmd.Root().Flags()
	changed := func(name string) bool { return flags.Changed(name) }

	effectiveDB, _ := flags.GetString("db")
	if effectiveDB == "" && !changed("db") && cfg.DB != "" {
		effectiveDB = cfg.DB
	}
	if effectiveDB == "" {
		effectiveDB = filepath.Join(contenoxDir, "local.db")
	}

	effectiveOllama, _ := flags.GetString("ollama")
	if effectiveOllama == defaultOllama && !changed("ollama") && cfg.Ollama != "" {
		effectiveOllama = cfg.Ollama
	}

	effectiveModel, _ := flags.GetString("model")
	if effectiveModel == defaultModel && !changed("model") && cfg.Model != "" {
		effectiveModel = cfg.Model
	}

	effectiveContext, _ := flags.GetInt("context")
	if effectiveContext == defaultContext && !changed("context") && cfg.Context != nil {
		effectiveContext = *cfg.Context
	}

	effectiveNoDeleteModels, _ := flags.GetBool("no-delete-models")
	if effectiveNoDeleteModels && !changed("no-delete-models") && cfg.NoDeleteModels != nil {
		effectiveNoDeleteModels = *cfg.NoDeleteModels
	}

	effectiveChain, _ := flags.GetString("chain")
	if effectiveChain == "" && !changed("chain") && cfg.DefaultChain != "" {
		effectiveChain = filepath.Join(contenoxDir, cfg.DefaultChain)
	}
	if effectiveChain == "" && !changed("chain") {
		wellKnown := filepath.Join(contenoxDir, "default-chain.json")
		if _, err := os.Stat(wellKnown); err == nil {
			effectiveChain = wellKnown
		}
	}
	if effectiveChain == "" {
		slog.Error("No chain file specified", "hint", "use --chain <path>, or set default_chain in .contenox/config.yaml, or add .contenox/default-chain.json")
		return errChainRequired
	}

	effectiveEnableLocalExec, _ := flags.GetBool("enable-local-exec")
	if !effectiveEnableLocalExec && !changed("enable-local-exec") && cfg.EnableLocalExec != nil {
		effectiveEnableLocalExec = *cfg.EnableLocalExec
	}

	effectiveLocalExecAllowedDir, _ := flags.GetString("local-exec-allowed-dir")
	if effectiveLocalExecAllowedDir == "" && !changed("local-exec-allowed-dir") && cfg.LocalExecAllowedDir != "" {
		effectiveLocalExecAllowedDir = cfg.LocalExecAllowedDir
	}

	effectiveLocalExecAllowedCommands, _ := flags.GetString("local-exec-allowed-commands")
	if effectiveLocalExecAllowedCommands == "" && !changed("local-exec-allowed-commands") && cfg.LocalExecAllowedCommands != "" {
		effectiveLocalExecAllowedCommands = cfg.LocalExecAllowedCommands
	}

	var effectiveLocalExecDeniedCommands []string
	if changed("local-exec-denied-commands") {
		denied, _ := flags.GetString("local-exec-denied-commands")
		effectiveLocalExecDeniedCommands = splitAndTrim(denied, ",")
	} else if len(cfg.LocalExecDeniedCommands) > 0 {
		effectiveLocalExecDeniedCommands = cfg.LocalExecDeniedCommands
	}

	effectiveTracing, _ := flags.GetBool("tracing")
	if !effectiveTracing && !changed("tracing") && cfg.Tracing != nil {
		effectiveTracing = *cfg.Tracing
	}

	effectiveSteps, _ := flags.GetBool("steps")
	if !effectiveSteps && !changed("steps") && cfg.Steps != nil {
		effectiveSteps = *cfg.Steps
	}

	effectiveRaw, _ := flags.GetBool("raw")
	if !effectiveRaw && !changed("raw") && cfg.Raw != nil {
		effectiveRaw = *cfg.Raw
	}

	resolvedBackends, effectiveDefaultProvider, effectiveDefaultModel := resolveEffectiveBackends(cfg, effectiveOllama, effectiveModel)
	if changed("model") {
		effectiveDefaultModel = effectiveModel
	}

	if effectiveEnableLocalExec && effectiveLocalExecAllowedDir != "" && effectiveLocalExecAllowedCommands != "" {
		allowedDir, err := filepath.Abs(effectiveLocalExecAllowedDir)
		if err != nil {
			slog.Error("Invalid allowed directory", "error", err)
			return err
		}
		commands := splitAndTrim(effectiveLocalExecAllowedCommands, ",")
		for _, c := range commands {
			if filepath.IsAbs(c) && !strings.HasPrefix(c, allowedDir) {
				slog.Error("Command path not inside allowed directory", "command", c, "allowed_dir", allowedDir)
				return errInvalidConfig
			}
		}
	}

	// Input: --input overrides positional args; else positionals joined; else empty (run() will try stdin).
	var inputValue string
	var inputPassed bool
	if changed("input") {
		inputValue, _ = flags.GetString("input")
		inputPassed = true
	} else if len(args) > 0 {
		inputValue = strings.Join(args, " ")
		inputPassed = true
	}

	timeout, _ := flags.GetDuration("timeout")
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
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
		InputValue:                        inputValue,
		InputFlagPassed:                   inputPassed,
		Cfg:                               cfg,
		ResolvedBackends:                  resolvedBackends,
		ContenoxDir:                       contenoxDir,
	}
	run(ctx, opts)
	return nil
}

// Sentinel errors so RunE can return and main can os.Exit(1).
var (
	errChainRequired = &exitError{1}
	errInvalidConfig = &exitError{1}
)

type exitError struct{ code int }

func (e *exitError) Error() string { return "exit" }

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
var reservedSubcommands = map[string]bool{"init": true, "run": true, "help": true, "completion": true, "session": true, "plan": true, "exec": true, "hook": true}

// Main runs the vibe CLI: init subcommand or run (default) with optional positional input.
func Main() {
	args := os.Args[1:]
	// Only inject "run" when no reserved subcommand was given (so "vibe completion" and "vibe help" work).
	// Scan past leading flags (e.g. --db /path) to find the first non-flag argument.
	// Also skip injection when args contains only --help/-h so the root command shows its own help.
	onlyHelp := len(args) == 0
	if !onlyHelp {
		allHelp := true
		for _, a := range args {
			if a != "--help" && a != "-h" {
				allHelp = false
				break
			}
		}
		onlyHelp = allHelp
	}
	if !onlyHelp && !firstNonFlagIsReserved(args) {
		rootCmd.SetArgs(append([]string{"run"}, args...))
	}
	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}

// firstNonFlagIsReserved scans args, skipping flags and their values, and returns
// true if the first positional argument is a reserved subcommand name.
func firstNonFlagIsReserved(args []string) bool {
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			// Explicit end of flags; next arg would be positional.
			if i+1 < len(args) {
				return reservedSubcommands[args[i+1]]
			}
			return false
		}
		if strings.HasPrefix(a, "--") {
			// Long flag: if it has no '=' it consumes the next token as its value.
			if !strings.Contains(a, "=") {
				i++ // skip value
			}
			continue
		}
		if strings.HasPrefix(a, "-") && len(a) > 1 {
			// Short flag: skip (simplified: assume it consumes next token if no value attached).
			if len(a) == 2 {
				i++ // skip value
			}
			continue
		}
		// First non-flag argument found.
		return reservedSubcommands[a]
	}
	return false
}

var rootCmd = &cobra.Command{
	Use:   "vibe",
	Short: "AI agent CLI: plan and execute tasks using your LLM of choice.",
	Long: `Vibe is a local AI agent CLI that plans and executes multi-step tasks on your
machine using filesystem and shell tools — driven by your LLM of choice.
No daemon, no cloud required. State is stored in SQLite.

  Quickstart:
    vibe init                         # scaffold .contenox/ with config + chain
    vibe list files in my home dir    # one-shot natural language → shell
    vibe plan new "some multi-step goal"  # create an autonomous plan
    vibe plan next --auto             # execute until done

  LLM providers (edit .contenox/config.yaml after 'vibe init'):
    Local (Ollama):  ollama serve && ollama pull qwen2.5:7b
    OpenAI:          set OPENAI_API_KEY, uncomment openai backend in config
    Gemini:          set GEMINI_API_KEY, uncomment gemini backend in config

  For vibe plan, the model MUST support tool calling.
  Run 'vibe init' and open .contenox/config.yaml for full provider examples.`,
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
	f.String("chain", "", "Path to a task chain JSON file. Chains define the LLM workflow: which model, which hooks, how to branch. Falls back to default_chain in config, then .contenox/default-chain.json")
	f.String("input", "", "Input for the chain (default: positional args or stdin if piped)")
	f.Bool("enable-local-exec", false, "Enable the local_shell hook (use only in trusted environments)")
	f.String("local-exec-allowed-dir", "", "If set, local_shell may only run scripts/binaries under this directory")
	f.String("local-exec-allowed-commands", "", "Comma-separated list of allowed executable paths/names for local_shell")
	f.String("local-exec-denied-commands", "", "Comma-separated list of denied executable basenames/paths for local_shell")
	f.Duration("timeout", defaultTimeout, "Maximum execution time (e.g., 5m, 1h)")
	f.Bool("trace", false, "Enable operation telemetry on stderr")

	f.Bool("steps", false, "Print execution steps after the result")
	f.Bool("raw", false, "Print full output (e.g. entire chat JSON)")

	rootCmd.AddCommand(initCmd, runCmd, sessionCmd, planCmd, execCmd, hookCmd)

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

	effectiveTracing, _ := flags.GetBool("trace")
	if !effectiveTracing && !changed("trace") && cfg.Tracing != nil {

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
	ctx, cancel := context.WithTimeout(libtracker.WithNewRequestID(context.Background()), timeout)
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		slog.Warn("Received interrupt, shutting down...")
		cancel()
	}()

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

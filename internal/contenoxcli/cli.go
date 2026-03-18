// cli.go holds the contenox CLI entrypoint (Main), default constants, flags, and merge logic.
package contenoxcli

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/contenox/contenox/libtracker"
	"github.com/contenox/contenox/runtimetypes"
	"github.com/spf13/cobra"
)

// Version is set at build time via -ldflags "-X github.com/contenox/contenox/internal/contenoxcli.Version=vX.Y.Z".
// Falls back to "dev" when building without the flag (e.g. go run).
var Version = "dev"

const localTenantID = "00000000-0000-0000-0000-000000000001"

const (
	defaultOllama  = "http://127.0.0.1:11434"
	defaultModel   = "qwen2.5:7b"
	defaultContext = 0
	defaultTimeout = 5 * time.Minute
)

// reservedSubcommands are first-arg names that must not be treated as run input (Cobra or our subcommands).
var reservedSubcommands = map[string]bool{"init": true, "chat": true, "vibe": true, "help": true, "completion": true, "session": true, "plan": true, "run": true, "hook": true, "mcp": true, "backend": true, "config": true, "model": true, "models": true}

// Main runs the contenox CLI: init subcommand or run (default) with optional positional input.
func Main() {
	args := os.Args[1:]
	// Only inject "run" when no reserved subcommand was given (so "contenox completion" and "contenox help" work).
	// Scan past leading flags (e.g. --db /path) to find the first non-flag argument.
	// Also skip injection when args contains only --help/-h so the root command shows its own help.
	onlyHelp := len(args) == 0
	if !onlyHelp {
		allRootFlags := true
		for _, a := range args {
			if a != "--help" && a != "-h" && a != "--version" && a != "-v" {
				allRootFlags = false
				break
			}
		}
		onlyHelp = allRootFlags
	}
	if !onlyHelp && !firstNonFlagIsReserved(args) {
		rootCmd.SetArgs(append([]string{"run"}, args...))
	}
	if err := rootCmd.Execute(); err != nil {
		// SilenceErrors is set, so cobra suppresses its own error printing.
		// We always print it here.
		fmt.Fprintf(os.Stderr, "Error: %s\n", err.Error())
		os.Exit(1)
	}
}

// firstNonFlagIsReserved scans args, skipping flags and their values, and returns
// true if the first positional argument is a reserved subcommand name.
func firstNonFlagIsReserved(args []string) bool {
	// Boolean flags that do NOT consume the next token as their value.
	// Without this list, `contenox --trace chat` would mistake "chat" for the
	// value of --trace and then forward it to the chat command as text input.
	boolFlags := map[string]bool{
		"--shell": true, "--trace": true, "--steps": true, "--raw": true,
		"--think": true, "--no-delete-models": true,
		"-h": true, "--help": true, "-v": true, "--version": true,
	}
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
			// Long flag: boolean flags and flag=value forms don't consume next token.
			if strings.Contains(a, "=") || boolFlags[a] {
				continue
			}
			i++ // this flag consumes the next token as its value
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
	Use:   "contenox",
	Short: "AI agent CLI: plan and execute tasks using your LLM of choice.",
	Long: `Contenox is a local AI agent CLI that plans and executes multi-step tasks on your
machine using filesystem and shell tools — driven by your LLM of choice.
No daemon, no cloud required. State is stored in SQLite.

  Quickstart:
    contenox init                          # scaffold .contenox/ with default chains
    contenox "list files in my home dir"   # one-shot natural language → shell
    contenox vibe                          # interactive TUI: chat + plans + shell
    contenox plan new "some multi-step goal"  # create an autonomous multi-step plan
    contenox plan next --auto              # execute plan steps until done

  Register an LLM backend:
    # Local (Ollama)
    ollama serve && ollama pull qwen2.5:7b
    contenox backend add local --type ollama
    # Then set your default:
    contenox config set default-model qwen2.5:7b

    # Google Gemini (no GPU required)
    contenox backend add gemini --type gemini --api-key-env GEMINI_API_KEY
    contenox config set default-model  gemini-2.5-flash
    contenox config set default-provider gemini

    # OpenAI
    contenox backend add openai --type openai --api-key-env OPENAI_API_KEY
    contenox config set default-model    gpt-4o-mini
    contenox config set default-provider openai

  Scope note:
    Backends and config are GLOBAL (stored in ~/.contenox/local.db).
    Chain files (.contenox/) are LOCAL to each project directory — like .git/.
    Run 'contenox init' once per project to create the local chain files.

  Note: contenox plan requires a model that supports tool calling.`,
	SilenceUsage:  true,
	SilenceErrors: true,
}

var chatCmd = &cobra.Command{
	Use:   "chat",
	Short: "Run a stateful chat session (default when no subcommand is given).",
	Long: `Send a message to the active chat session and get a response.
Input is passed as positional args, --input, or piped via stdin.

  contenox "what can you do?"
  echo "summarise README.md" | contenox
  contenox chat --shell "list files in the current dir"

Sessions persist conversation history across invocations (stored in SQLite).
Each session remembers previous messages so the model has context.
The first run auto-creates a "default" session. Manage sessions with:

  contenox session list              list all sessions (* = active)
  contenox session new <name>        create a new named session (becomes active)
  contenox session switch <name>     switch to a different session
  contenox session show              print the active session's full history
  contenox session delete <name>     delete a session and all its messages

Giving the model tools (file system and shell access):

  --local-exec-allowed-dir <dir>     allow local_fs tools inside <dir>
  --shell                            enable local_shell (command policy is defined in the chain)

Examples:
  # Chat with file system access to the current project:
  contenox chat --local-exec-allowed-dir . "summarise the README"

  # Shell access (policy comes from the chain's hook_policies; default chains allow common dev tools):
  contenox chat --shell "suggest a commit message from git diff"

  # Trim context: only send last 10 messages from session history to the model:
  contenox chat --trim 10 "let's continue where we left off"

  # Show last 6 turns of the conversation after the reply:
  contenox chat --last 6 "hello"`,
	Args: cobra.ArbitraryArgs,
	RunE: runChat,
}

var initCmd = &cobra.Command{
	Use:   "init [provider]",
	Short: "Scaffold .contenox/ with default chain files.",
	Long: `Create the .contenox/ directory and populate it with default chain files.

After init, register a backend and set your default model:

  # Local Ollama:
  contenox backend add local --type ollama
  contenox config set default-model qwen2.5:7b

  # OpenAI:
  contenox backend add openai --type openai --api-key-env OPENAI_API_KEY
  contenox config set default-model gpt-5-mini

  # Google Gemini:
  contenox backend add gemini --type gemini --api-key-env GEMINI_API_KEY
  contenox config set default-model gemini-3.1-pro-preview

Use --force to overwrite existing files.`,
	Args: cobra.MaximumNArgs(1),
	RunE: runInitCmd,
}

func init() {
	// Wire the ldflags-injected version into cobra's built-in --version/-v flag.
	// Must be done here (not in the struct literal) so the ldflags value is used.
	rootCmd.Version = Version

	// Run flags on root so "contenox --input x" and "contenox hi" both work.
	f := rootCmd.PersistentFlags()
	f.String("db", "", "SQLite database path (default: .contenox/local.db)")
	f.String("ollama", defaultOllama, "Ollama base URL")
	f.String("model", defaultModel, "Model name (task/chat/embed)")
	f.String("provider", "", "Provider type override (ollama, openai, vllm, gemini). Overrides config default_provider.")
	f.Int("context", defaultContext, "Context length")
	f.Bool("no-delete-models", true, "Do not delete Ollama models that are not declared (default true for contenox)")
	f.String("chain", "", "Path to a task chain JSON file. Chains define the LLM workflow: which model, which hooks, how to branch. Falls back to default_chain in config, then .contenox/default-chain.json")
	f.String("input", "", "Input for the chain (default: positional args or stdin if piped)")
	f.Bool("shell", false, "Enable the local_shell hook (use only in trusted environments)")
	f.String("local-exec-allowed-dir", "", "If set, local_shell may only run scripts/binaries under this directory")
	f.Duration("timeout", defaultTimeout, "Maximum execution time (e.g., 5m, 1h)")
	f.Bool("trace", false, "Enable operation telemetry on stderr")

	f.Bool("steps", false, "Print execution steps after the result")
	f.Bool("raw", false, "Print full output (e.g. entire chat JSON)")
	f.Bool("think", false, "Print model reasoning trace to stderr (for thinking models)")

	rootCmd.AddCommand(initCmd, chatCmd, sessionCmd, planCmd, runCmd, hookCmd)
	rootCmd.AddCommand(mcpCmd)
	rootCmd.AddCommand(backendCmd)
	rootCmd.AddCommand(configCmd)
	rootCmd.AddCommand(modelCmd)

	rootCmd.InitDefaultHelpCmd() // so "contenox help" is handled by Cobra, not passed as run input
	initCmd.Flags().BoolP("force", "f", false, "Overwrite existing files")

	// Chat-specific local flags (not exposed globally).
	chatCmd.Flags().Int("trim", 0, "Only send the last N messages from session history to the model (0 = send all)")
	chatCmd.Flags().Int("last", 0, "Print last N user/assistant turns after the reply (0 = only print new reply)")
}

// ResolveContenoxDir finds the closest .contenox directory by walking up from the
// current working directory. If it hits the root without finding one, it returns
// the .contenox directory in the current working directory as a fallback.
func ResolveContenoxDir() (string, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return "", err
	}

	dir := cwd
	for {
		candidate := filepath.Join(dir, ".contenox")
		if info, err := os.Stat(candidate); err == nil && info.IsDir() {
			return candidate, nil
		}

		parent := filepath.Dir(dir)
		if parent == dir {
			// Hit root without finding it. Fallback to cwd/.contenox
			return filepath.Join(cwd, ".contenox"), nil
		}
		dir = parent
	}
}

func runInitCmd(cmd *cobra.Command, args []string) error {
	force, _ := cmd.Flags().GetBool("force")
	provider := ""
	if len(args) > 0 {
		provider = args[0]
	}
	return RunInit(cmd.OutOrStdout(), cmd.ErrOrStderr(), force, provider)
}

func runChat(cmd *cobra.Command, args []string) error {
	// No subcommand, no input, and no piped stdin: show help and exit 0.
	flags := cmd.Root().Flags()
	if len(args) == 0 && !flags.Changed("input") {
		if stat, err := os.Stdin.Stat(); err != nil || (stat.Mode()&os.ModeCharDevice) != 0 {
			_ = cmd.Root().Usage()
			return nil
		}
	}

	contenoxDir, err := ResolveContenoxDir()
	if err != nil {
		return fmt.Errorf("failed to resolve .contenox dir: %w", err)
	}

	// Resolve DB path (needed for KV reads below).
	dbPath, err := resolveDBPath(cmd)
	if err != nil {
		return err
	}
	dbCtx := libtracker.WithNewRequestID(context.Background())
	db, err := openDBAt(dbCtx, dbPath)
	if err != nil {
		return err
	}
	defer db.Close()

	store := runtimetypes.New(db.WithoutTransaction())

	changed := func(name string) bool { return flags.Changed(name) }

	// Resolve model: flag > SQLite KV > hardcoded default.
	effectiveModel, _ := flags.GetString("model")
	if !changed("model") || effectiveModel == defaultModel {
		if kv, _ := getConfigKV(dbCtx, store, "default-model"); kv != "" {
			effectiveModel = kv
		}
	}

	effectiveDefaultProvider := ""
	if kv, _ := getConfigKV(dbCtx, store, "default-provider"); kv != "" {
		effectiveDefaultProvider = kv
	}
	if changed("provider") {
		effectiveDefaultProvider, _ = flags.GetString("provider")
	}

	effectiveContext, _ := flags.GetInt("context")
	effectiveNoDeleteModels, _ := flags.GetBool("no-delete-models")

	// Resolve chain: flag > SQLite KV default-chain > well-known file.
	effectiveChain, _ := flags.GetString("chain")
	if effectiveChain == "" && !changed("chain") {
		if kv, _ := getConfigKV(dbCtx, store, "default-chain"); kv != "" {
			effectiveChain = kv
			if !filepath.IsAbs(effectiveChain) {
				effectiveChain = filepath.Join(contenoxDir, effectiveChain)
			}
		}
	}
	if effectiveChain == "" && !changed("chain") {
		wellKnown := filepath.Join(contenoxDir, "default-chain.json")
		if _, err := os.Stat(wellKnown); err == nil {
			effectiveChain = wellKnown
		}
	}
	if effectiveChain == "" {
		// No chain found anywhere in the directory tree — guide the user.
		fmt.Fprintln(os.Stderr, "No .contenox/ project found in this directory or any parent directory.")
		fmt.Fprintln(os.Stderr, "Run 'contenox init' to get started, or pass --chain explicitly.")
		return errChainRequired
	}

	effectiveEnableLocalExec, _ := flags.GetBool("shell")
	effectiveLocalExecAllowedDir, _ := flags.GetString("local-exec-allowed-dir")

	effectiveTracing, _ := flags.GetBool("trace")
	effectiveSteps, _ := flags.GetBool("steps")
	effectiveRaw, _ := flags.GetBool("raw")

	var inputValue string
	var inputPassed bool
	if changed("input") {
		inputValue, _ = flags.GetString("input")
		inputPassed = true
	} else if len(args) > 0 {
		inputValue = strings.Join(args, " ")
	}

	timeout, _ := flags.GetDuration("timeout")
	ctx, cancel := context.WithTimeout(libtracker.WithNewRequestID(context.Background()), timeout)
	defer cancel()

	// Use signal.NotifyContext so cleanup is automatic when the cmd returns;
	// avoids leaking a goroutine blocked forever on <-sigCh.
	ctx, cancel = signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	effectiveThink, _ := flags.GetBool("think")
	historyTrim, _ := cmd.Flags().GetInt("trim")
	lastN, _ := cmd.Flags().GetInt("last")

	opts := chatOpts{
		EffectiveDB:                       dbPath,
		EffectiveChain:                    effectiveChain,
		EffectiveDefaultModel:             effectiveModel,
		EffectiveDefaultProvider:          effectiveDefaultProvider,
		EffectiveContext:                  effectiveContext,
		EffectiveNoDeleteModels:           effectiveNoDeleteModels,
		EffectiveEnableLocalExec:          effectiveEnableLocalExec,
		EffectiveLocalExecAllowedDir:      effectiveLocalExecAllowedDir,
		EffectiveTracing:                  effectiveTracing,
		EffectiveSteps:                    effectiveSteps,
		EffectiveRaw:                      effectiveRaw,
		EffectiveThink:                    effectiveThink,
		HistoryTrim:                       historyTrim,
		LastN:                             lastN,
		InputValue:                        inputValue,
		InputFlagPassed:                   inputPassed,
		ContenoxDir:                       contenoxDir,
	}
	// Gap 7: quick pre-flight hint for the obvious case: model was explicitly set
	// to something non-default, but provider was never configured.
	if effectiveDefaultProvider == "" && effectiveModel != defaultModel {
		fmt.Fprintln(os.Stderr, "default-provider is not set.")
		fmt.Fprintln(os.Stderr, "Run: contenox config set default-provider <ollama|gemini|openai>")
		return errInvalidConfig
	}
	if err := execChat(ctx, db, opts, cmd.OutOrStdout(), cmd.ErrOrStderr()); err != nil {
		// Gap 7 fallback: when the engine can't find any model and provider is
		// not configured, the root cause is almost always a missing default-provider.
		if effectiveDefaultProvider == "" &&
			(strings.Contains(err.Error(), "no models found") ||
				strings.Contains(err.Error(), "model name cannot be empty") ||
				strings.Contains(err.Error(), "client resolution failed")) {
			fmt.Fprintln(os.Stderr, "")
			fmt.Fprintln(os.Stderr, "Hint: default-provider is not set. If you are using Gemini or OpenAI, run:")
			fmt.Fprintln(os.Stderr, "  contenox config set default-provider <ollama|gemini|openai>")
		}
		return err
	}
	return nil
}

// Sentinel errors so RunE can return and main can os.Exit(1).
var (
	errChainRequired = &exitError{1}
	errInvalidConfig = &exitError{1}
)

type exitError struct{ code int }

func (e *exitError) Error() string { return "exit" }

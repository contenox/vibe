// cli.go holds the contenox CLI entrypoint (Main), default constants, flags, and merge logic.
package contenoxcli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/contenox/beam/internal/kernel/reasoning"
	"github.com/contenox/beam/internal/libtracker"
	"github.com/contenox/beam/internal/models/modelrepo"
	"github.com/contenox/beam/internal/services/clikv"
	"github.com/contenox/beam/internal/services/project"
	"github.com/contenox/beam/internal/store/runtimetypes"
	"github.com/contenox/beam/internal/version"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// Version is an optional link-time override via
// -ldflags "-X github.com/contenox/beam/internal/surfaces/contenoxcli.Version=…"
// (e.g. distro packagers). When empty, CLIVersion uses runtime/version/version.txt.
var Version string

// CLIVersion returns the effective CLI version string (embedded file or link override).
func CLIVersion() string {
	return cliVersion()
}

func cliVersion() string {
	if v := strings.TrimSpace(Version); v != "" {
		return v
	}
	return version.Get()
}

const DefaultWorkspaceID = "00000000-0000-0000-0000-000000000002"

const (
	defaultOllama  = "http://127.0.0.1:11434"
	defaultModel   = "qwen2.5:7b"
	defaultContext = 0
	defaultTimeout = 5 * time.Minute
)

// reservedSubcommands are first-arg names that must not be treated as run input
// (Cobra or our subcommands). RETIRED command names (serve, fleet, approvals,
// code, vscode-agent, modeld) stay reserved on purpose: an operator typing one
// gets Cobra's unknown-command error naming the mistake, instead of the word
// being silently injected as a chat prompt.
var reservedSubcommands = map[string]bool{"init": true, "chat": true, "help": true, "completion": true, "session": true, "run": true, "tools": true, "mcp": true, "backend": true, "agent": true, "config": true, "model": true, "models": true, "doctor": true, "version": true, "state": true, "acp": true, "acpx": true, "setup": true, "cache": true, "update": true, "workspace": true, "sandbox": true, "shell-env": true, "vet": true, "serve": true, "fleet": true, "mission": true, "approvals": true, "code": true, "vscode-agent": true, "modeld": true, "beam": true}

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
	if sub := dispatchSubcommand(args, onlyHelp); sub != "" {
		rootCmd.SetArgs(append([]string{sub}, args...))
	}
	err := rootCmd.Execute()
	// Flush warm-session KV snapshots to the durable store before the process
	// exits, so the next invocation (one-shot CLI) or the next start (a restarted
	// server) can restore warm instead of cold-prefilling. Best-effort; a capture
	// failure must never change the command's exit status.
	_ = modelrepo.Shutdown()
	if err != nil {
		recordStartupFailure(err)
		// A command that chose its own exit status (an *exitError) has already
		// written whatever it wanted the operator to see — `mission fire --wait`
		// prints its terminal-outcome line, the chain/editor sentinels print
		// their own guidance. Honor the code and skip the generic "Error:" prefix
		// so a deliberate non-zero exit (a mission that failed, blocked, or timed
		// out) reads as a status, not a crash. Every pre-existing exitError uses
		// code 1, so this changes nothing for them but the cosmetic prefix.
		var ee *exitError
		if errors.As(err, &ee) {
			os.Exit(ee.code)
		}
		fmt.Fprintf(os.Stderr, "Error: %s\n", err.Error())
		os.Exit(1)
	}
}

func containsExperimentalACPFlag(args []string) bool {
	return slices.Contains(args, "--experimental-acp")
}

// dispatchSubcommand decides which subcommand a bare invocation is routed to.
// Bare input is session-backed chat, as the quickstart and chat help document
// ("the first run auto-creates a 'default' session"); 'contenox run' remains
// the explicit stateless path. Returns "" when args already name a subcommand
// or are help/version-only.
func dispatchSubcommand(args []string, onlyHelp bool) string {
	switch {
	case containsExperimentalACPFlag(args) && !firstNonFlagIsReserved(args):
		return "acp"
	case !onlyHelp && !firstNonFlagIsReserved(args):
		return "chat"
	}
	return ""
}

func recordStartupFailure(execErr error) {
	defer func() { _ = recover() }()
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return
	}
	dir := filepath.Join(home, ".contenox")
	if mkErr := os.MkdirAll(dir, 0o755); mkErr != nil {
		return
	}
	f, openErr := os.OpenFile(filepath.Join(dir, "telemetry.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if openErr != nil {
		return
	}
	defer f.Close()
	logger := slog.New(slog.NewTextHandler(f, &slog.HandlerOptions{Level: slog.LevelInfo}))
	tr := libtracker.NewLogActivityTracker(logger)
	reportErr, _, end := tr.Start(context.Background(), "exec", "cli",
		"argv", strings.Join(os.Args[1:], " "),
		"version", CLIVersion(),
	)
	reportErr(execErr)
	end()
}

// firstNonFlagIsReserved scans args, skipping flags and their values, and returns
// true if the first positional argument is a reserved subcommand name.
func firstNonFlagIsReserved(args []string) bool {
	// Boolean flags that do NOT consume the next token as their value.
	// Without this list, `contenox --trace chat` would mistake "chat" for the
	// value of --trace and then forward it to the chat command as text input.
	boolFlags := map[string]bool{
		"--shell": true, "--trace": true, "--steps": true, "--raw": true,
		"--no-delete-models": true, "--editor": true,
		"-e": true, "-h": true, "--help": true, "-v": true, "--version": true,
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
	Short: "An open coding harness: chat, chains, and missions from your terminal.",
	Long: `Contenox is an open coding harness. Chat and shell in your terminal, use
the same harness from any ACP editor, and package repeatable work into chains —
prompts, model routing, tools, retries, and approval gates in one versioned
file. State lives in local SQLite. Hosted providers and Ollama work out of the
box; for local inference run Ollama or vLLM.

  Quickstart:
    contenox setup                         # interactive wizard — pick provider, model, API key
    contenox init                          # scaffold .contenox/ with default chains
    contenox "list files in my home dir"   # session-backed chat using your configured policy

  Inspect models:
    contenox model list                    # models exposed by registered live backends

  Or register an LLM backend manually:
    # Local Ollama daemon
    ollama serve && ollama pull qwen2.5:7b
    contenox backend add ollama --type ollama
    contenox config set default-provider ollama
    contenox config set default-model qwen2.5:7b

    # Hosted Ollama Cloud
    contenox backend add ollama-cloud --type ollama --url https://ollama.com/api --api-key-env OLLAMA_API_KEY
    contenox config set default-provider ollama

    # Google Gemini (no GPU required)
    contenox backend add gemini --type gemini --api-key-env GEMINI_API_KEY
    contenox config set default-model  gemini-flash-latest
    contenox config set default-provider gemini

    # OpenAI
    contenox backend add openai --type openai --api-key-env OPENAI_API_KEY
    contenox config set default-model    gpt-4o-mini
    contenox config set default-provider openai

  VS Code autocomplete can use a separate model from chat:
    # Example: chat on OpenAI, ghost text on local Ollama.
    contenox config set default-provider openai
    contenox config set default-model    gpt-5-mini
    contenox config set default-autocomplete-provider ollama
    contenox config set default-autocomplete-model    qwen2.5-coder:7b

  Scope note:
    Backends and config are GLOBAL (stored in ~/.contenox/local.db).
    Chain files (.contenox/) are LOCAL to each project directory — like .git/.
    Run 'contenox init' once per project to create the local chain files.`,
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

  contenox session list              list active-scope sessions (* = active)
  contenox session list --all        list every session across the whole DB
  contenox session new <name>        create a new named session (becomes active)
  contenox session switch <name>     switch to a different session
  contenox session show [name|id]    print a session (active, by name, or by id)
  contenox session delete <name>     delete a session and all its messages
  contenox session workspaces        list workspaces and namespaces (whole DB)
  contenox session fork --summary    compact older history into a summary and continue
                                     in a new session (useful when context fills up)

Giving the model tools (file system and shell access):

  --local-exec-allowed-dir <dir>     allow local_fs tools inside <dir>
  --shell                            enable local_shell (command policy is defined in the chain)

Human-in-the-loop is on by default. The workflow pauses for terminal approval before
write_file, sed, and local_shell calls. The active policy is defined in
~/.contenox/hitl-policy-default.json (override per workspace via
.contenox/hitl-policy-*.json or via 'contenox config set hitl-policy-name').

  --auto                             non-interactive mode: skip approval prompts
                                     entirely. Use only in trusted environments
                                     or for scripted workflows.

Examples:
  # Chat with file system access to the current project:
  contenox chat --local-exec-allowed-dir . "summarise the README"

  # Shell access (policy comes from the chain's tools_policies; default chains allow common dev tools):
  contenox chat --shell "suggest a commit message from git diff"

  # Non-interactive shell run — no approvals, runs everything allowed by policy (USE WITH CARE):
  contenox chat --shell --local-exec-allowed-dir . --auto "refactor main.go to use slog"

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

This writes default-chain.json and default-run-chain.json.

After init, register a backend, make sure the runtime can see a model, then set your defaults:

  # Local Ollama:
  contenox backend add local --type ollama
  contenox config set default-provider ollama
  contenox config set default-model qwen2.5:7b

  # Hosted Ollama Cloud:
  contenox backend add ollama-cloud --type ollama --url https://ollama.com/api --api-key-env OLLAMA_API_KEY
  contenox config set default-provider ollama
  contenox config set default-model gpt-oss:20b

  # OpenAI:
  contenox backend add openai --type openai --api-key-env OPENAI_API_KEY
  contenox config set default-provider openai
  contenox config set default-model gpt-5-mini

  # Google Gemini:
  contenox backend add gemini --type gemini --api-key-env GEMINI_API_KEY
  contenox config set default-provider gemini
  contenox config set default-model gemini-3.1-pro-preview

  # Optional VS Code autocomplete model, independent from chat:
  contenox config set default-autocomplete-provider ollama
  contenox config set default-autocomplete-model qwen2.5-coder:7b

Use --force to overwrite existing files, or --update to refresh unchanged default files to the latest version.`,
	Args: cobra.MaximumNArgs(1),
	RunE: runInitCmd,
}

// versionCmd prints the same line as `contenox --version` so `contenox version`
// is not mistaken for chat input (the default run command).
var versionCmd = &cobra.Command{
	Use:   "version",
	Short: "Print the contenox CLI version",
	Long: `Print the contenox CLI version string.

This is the subcommand form of 'contenox --version' and exists so that typing
'contenox version' is not mistaken for chat input by the default run command.`,
	Run: func(cmd *cobra.Command, args []string) {
		fmt.Printf("%s version %s\n", cmd.Root().Name(), cmd.Root().Version)
	},
}

func init() {
	v := cliVersion()
	rootCmd.Version = v
	rootCmd.Short = fmt.Sprintf("Local AI workflow runtime v%s: run versioned chains with your tools and models.", v)
	// Cobra prints Long for --help when set; include version so it matches runtime/version/version.txt.
	rootCmd.Long = fmt.Sprintf("Version: %s\n\n%s", v, rootCmd.Long)

	// Run flags on root so "contenox --input x" and "contenox hi" both work.
	f := rootCmd.PersistentFlags()
	f.String("db", "", "SQLite database path (default: ~/.contenox/local.db)")
	f.String("data-dir", "", "Override the .contenox data directory path")
	f.String("ollama", defaultOllama, "Ollama base URL")
	f.String("model", defaultModel, "Model name (task/chat/embed)")
	f.String("provider", "", "Provider type override. See 'contenox backend add --help' for supported backend types.")
	f.String("alt-model", "", "Alt model name (chains referencing {{var:alt_model}}). Overrides config default-alt-model.")
	f.String("alt-provider", "", "Alt provider type (chains referencing {{var:alt_provider}}). Overrides config default-alt-provider.")
	f.Int("max-tokens", 0, "Response token cap for chains referencing {{var:max_tokens}}. Overrides config default-max-tokens when set.")
	f.Int("context", defaultContext, "Context length")
	f.Bool("no-delete-models", true, "Legacy compatibility flag; OSS runtime model deletion is disabled.")
	f.String("chain", "", "Path to a task chain JSON file. Chains define the LLM workflow: which model, which tools, how to branch. Falls back to default_chain in config, then .contenox/default-chain.json")
	f.String("input", "", "Input for the chain (default: positional args or stdin if piped)")
	f.Bool("shell", false, "Enable the local_shell tools (use only in trusted environments)")
	f.String("local-exec-allowed-dir", "", "If set, local_shell may only run scripts/binaries under this directory")
	f.Duration("timeout", defaultTimeout, "Maximum execution time (e.g., 5m, 1h)")
	f.Bool("trace", false, "Stream task-step events to stderr in real time")

	f.Bool("steps", false, "Print execution steps after the result")
	f.Bool("raw", false, "Print full output (e.g. entire chat JSON)")
	f.String("think", "", "Set reasoning level for supported models: auto, off, minimal, low, medium, high, xhigh (default: config default-think, then high)")
	f.BoolP("editor", "e", false, "Open $EDITOR (or $VISUAL, fallback nano) to compose the prompt; piped stdin is preloaded as reference")

	rootCmd.AddCommand(initCmd, chatCmd, sessionCmd, runCmd, toolsCmd, doctorCmd, versionCmd)
	rootCmd.AddCommand(mcpCmd)
	rootCmd.AddCommand(backendCmd)
	rootCmd.AddCommand(agentCmd)
	rootCmd.AddCommand(missionCmd)
	rootCmd.AddCommand(cacheCmd)
	rootCmd.AddCommand(configCmd)
	rootCmd.AddCommand(modelCmd)
	rootCmd.AddCommand(stateCmd)
	rootCmd.AddCommand(acpCmd)
	rootCmd.AddCommand(acpxCmd)
	rootCmd.AddCommand(setupCmd)
	rootCmd.AddCommand(updateCmd)
	rootCmd.AddCommand(workspaceCmd)
	rootCmd.AddCommand(sandboxCmd)
	rootCmd.AddCommand(shellEnvCmd)
	rootCmd.AddCommand(vetCmd)

	rootCmd.InitDefaultHelpCmd() // so "contenox help" is handled by Cobra, not passed as run input
	initCmd.Flags().BoolP("force", "f", false, "Overwrite existing files")
	initCmd.Flags().Bool("update", false, "Update unchanged default files to the latest version")
	initCmd.Flags().Bool("project", false, "Create a project marker in the CURRENT directory (a fresh workspace id), instead of reusing an ancestor's .contenox")
	initCmd.Flags().String("name", "", "Friendly project name for the marker (default: the directory name)")

	// Chat-specific local flags (not exposed globally).
	chatCmd.Flags().Int("trim", 0, "Only send the last N messages from session history to the model (0 = send all)")
	chatCmd.Flags().StringArray("attach", nil, "Attach an image to this message (repeatable). Routes to a vision-capable model.")
	chatCmd.Flags().Int("last", 0, "Print last N user/assistant turns after the reply (0 = only print new reply)")
	chatCmd.Flags().Bool("auto", false, "Non-interactive mode: disable HITL approval prompts. Default is HITL on; tools route through the active hitl-policy. Use --auto only in trusted/scripted contexts.")

}

// setupTelemetryLogging checks if the user has enabled file logging.
// If enabled, it sets up slog to write to both os.Stderr and ~/.contenox/telemetry.log.
// Returns a cleanup function to close the file.
func setupTelemetryLogging(ctx context.Context, store runtimetypes.Store, contenoxDir string) (func(), error) {
	enabledStr := clikv.Read(ctx, store, "telemetry-enabled")
	if enabledStr != "true" {
		return func() {}, nil
	}

	logPath := filepath.Join(contenoxDir, "telemetry.log")
	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return func() {}, fmt.Errorf("failed to open telemetry log: %w", err)
	}

	mw := io.MultiWriter(os.Stderr, f)
	slog.SetDefault(slog.New(slog.NewTextHandler(mw, &slog.HandlerOptions{Level: slog.LevelInfo})))
	return func() { f.Close() }, nil
}

// ResolveContenoxDir finds the closest .contenox directory by walking up from the
// current working directory. If cmd is non-nil and --data-dir is set, that value
// is returned directly. Otherwise it walks up from cwd; if it hits the root
// without finding one, it returns the .contenox directory in the current working
// directory as a fallback.
func ResolveContenoxDir(cmd *cobra.Command) (string, error) {
	if cmd != nil {
		dataDir, _ := cmd.Root().PersistentFlags().GetString("data-dir")
		if dataDir != "" {
			return filepath.Abs(dataDir)
		}
	}

	cwd, err := os.Getwd()
	if err != nil {
		return "", err
	}

	dir := cwd
	for {
		candidate := filepath.Join(dir, ".contenox")
		if info, err := os.Stat(candidate); err == nil && info.IsDir() {
			// Require workspace.id to be present — a .contenox/ without it is
			// not a valid workspace (e.g. a backup or pre-init directory).
			// Keep walking up so callers get a proper workspace or the cwd fallback.
			if _, werr := os.Stat(filepath.Join(candidate, "workspace.id")); werr == nil {
				return candidate, nil
			}
		}

		parent := filepath.Dir(dir)
		if parent == dir {
			// Hit root without finding it. Fallback to cwd/.contenox
			return filepath.Join(cwd, ".contenox"), nil
		}
		dir = parent
	}
}

// controlPlaneDirs is the SINGLE definition of "what the runtime's control plane
// is" for a given command: the resolved .contenox data/config dir (contenoxDir —
// chains, HITL policies, declared agents, and, unless split by --db, the local.db)
// PLUS the home global ~/.contenox (the default database and the model cache).
// These are the directories no session, browse root, or agent fs tool may reach,
// per the control-plane isolation invariant (runtime/vfs/controlplane.go).
//
// Both serve (which calls vfs.SetControlPlaneDenied at boot) and the
// `contenox workspace add` grant guard (which runs in a SEPARATE process with no
// global set, so it consults vfs.WithinControlPlane with these dirs directly)
// derive the set from here, so the two never disagree about the boundary. --data-dir
// may point contenoxDir away from home; both are denied, so the split is covered
// either way. NOTE: a database relocated by --db to an arbitrary directory is an
// operator's explicit choice and is NOT auto-denied — denying its parent could
// swallow a legitimate workspace root (e.g. --db ./local.db in the served project);
// the two canonical control dirs are the boundary the invariant protects.
func controlPlaneDirs(contenoxDir string) []string {
	dirs := []string{contenoxDir}
	if home, err := globalContenoxDir(); err == nil {
		dirs = append(dirs, home)
	}
	return dirs
}

func ResolveWorkspaceID(contenoxDir string) string {
	// The project package owns the marker format (JSON {id,name}, with a
	// legacy bare-UUID read) so serve, the CLI, and the /workspace/roots API
	// agree on a project's identity. The DB scoping token is the marker's ID.
	if m, ok := project.ReadFromContenoxDir(contenoxDir); ok && m.ID != "" {
		return m.ID
	}
	return DefaultWorkspaceID
}

func runInitCmd(cmd *cobra.Command, args []string) error {
	force, _ := cmd.Flags().GetBool("force")
	update, _ := cmd.Flags().GetBool("update")
	projectMode, _ := cmd.Flags().GetBool("project")
	rawName, _ := cmd.Flags().GetString("name")
	projectName, err := project.NormalizeName(rawName)
	if err != nil {
		return err
	}
	provider := ""
	if len(args) > 0 {
		provider = args[0]
	}

	var contenoxDir string
	if projectMode {
		// Force a LOCAL project marker in the current directory, bypassing the
		// git-style walk-up that would otherwise reuse an ancestor's .contenox.
		cwd, err := os.Getwd()
		if err != nil {
			return fmt.Errorf("failed to resolve current directory: %w", err)
		}
		contenoxDir, projectName = resolveProjectInit(cwd, projectName)
		if err := RunInit(cmd.OutOrStdout(), cmd.ErrOrStderr(), force, update, provider, contenoxDir, projectName); err != nil {
			return err
		}
		// Marking a project does NOT grant it as a workspace root — that is a
		// separate, explicit security decision. Bridge the journey with the verb
		// that registers it for serve and the Beam picker.
		fmt.Fprintf(cmd.OutOrStdout(),
			"To let sessions open it (serve + Beam picker): contenox workspace add %s\n", cwd)
		return nil
	}
	contenoxDir, err = ResolveContenoxDir(cmd)
	if err != nil {
		return fmt.Errorf("failed to resolve .contenox dir: %w", err)
	}
	return RunInit(cmd.OutOrStdout(), cmd.ErrOrStderr(), force, update, provider, contenoxDir, projectName)
}

// resolveProjectInit computes the target .contenox dir and effective marker name
// for `init --project`: a LOCAL marker in cwd (bypassing the ancestor walk-up in
// ResolveContenoxDir), always named — defaulting to the project directory's
// basename when no explicit --name is given.
func resolveProjectInit(cwd, name string) (contenoxDir, projectName string) {
	contenoxDir = filepath.Join(cwd, project.ContenoxDirName)
	projectName = name
	if strings.TrimSpace(projectName) == "" {
		projectName = filepath.Base(filepath.Dir(contenoxDir))
	}
	return contenoxDir, projectName
}

func runChat(cmd *cobra.Command, args []string) error {
	flags := cmd.Root().Flags()
	useEditor, _ := flags.GetBool("editor")

	if len(args) == 1 && args[0] == "help" && !flags.Changed("input") && !useEditor {
		_ = cmd.Help()
		return nil
	}

	// No subcommand, no input, no editor, and no piped stdin: show help and exit 0.
	if len(args) == 0 && !flags.Changed("input") && !useEditor {
		if stat, err := os.Stdin.Stat(); err != nil || (stat.Mode()&os.ModeCharDevice) != 0 {
			_ = cmd.Usage()
			return nil
		}
	}

	contenoxDir, err := ResolveContenoxDir(cmd)
	if err != nil {
		return fmt.Errorf("failed to resolve .contenox dir: %w", err)
	}

	// Resolve DB path (needed for KV reads below).
	dbPath, err := resolveDBPath(cmd)
	if err != nil {
		return err
	}
	dbCtx := libtracker.WithNewRequestID(context.Background())
	db, err := OpenDBAt(dbCtx, dbPath)
	if err != nil {
		return err
	}
	defer db.Close()

	closeLogs, err := setupTelemetryLogging(dbCtx, runtimetypes.New(db.WithoutTransaction()), contenoxDir)
	if err != nil {
		slog.Warn("Failed to setup telemetry logging", "error", err)
	}
	defer closeLogs()

	store := runtimetypes.New(db.WithoutTransaction())

	changed := func(name string) bool { return flags.Changed(name) }

	configuredDefaultModel := ""
	if kv, _ := getConfigKV(dbCtx, store, "default-model"); kv != "" {
		configuredDefaultModel = kv
	}
	configuredDefaultProvider := ""
	if kv, _ := getConfigKV(dbCtx, store, "default-provider"); kv != "" {
		configuredDefaultProvider = kv
	}

	// Resolve model: flag > SQLite KV > hardcoded default.
	effectiveModel, _ := flags.GetString("model")
	if !changed("model") || effectiveModel == defaultModel {
		if configuredDefaultModel != "" {
			effectiveModel = configuredDefaultModel
		}
	}

	effectiveDefaultProvider := configuredDefaultProvider
	if changed("provider") {
		effectiveDefaultProvider, _ = flags.GetString("provider")
	}

	effectiveAltModel := ""
	if kv, _ := getConfigKV(dbCtx, store, "default-alt-model"); kv != "" {
		effectiveAltModel = kv
	}
	if changed("alt-model") {
		effectiveAltModel, _ = flags.GetString("alt-model")
	}

	effectiveAltProvider := ""
	if kv, _ := getConfigKV(dbCtx, store, "default-alt-provider"); kv != "" {
		effectiveAltProvider = kv
	}
	if changed("alt-provider") {
		effectiveAltProvider, _ = flags.GetString("alt-provider")
	}

	effectiveMaxTokens, err := resolveEffectiveMaxTokens(dbCtx, store, flags)
	if err != nil {
		return err
	}

	effectiveContext, _ := flags.GetInt("context")
	effectiveNoDeleteModels, _ := flags.GetBool("no-delete-models")

	effectiveChain, _ := flags.GetString("chain")
	if effectiveChain == "" && !changed("chain") {
		if kv, _ := getConfigKV(dbCtx, store, "default-chain"); kv != "" {
			effectiveChain = kv
			if !filepath.IsAbs(effectiveChain) {
				if resolved, rerr := lookupSystemFile(contenoxDir, effectiveChain); rerr == nil {
					effectiveChain = resolved
				} else {
					effectiveChain = filepath.Join(contenoxDir, effectiveChain)
				}
			}
		}
	}
	if effectiveChain == "" && !changed("chain") {
		if resolved, rerr := lookupSystemFile(contenoxDir, "default-chain.json"); rerr == nil {
			effectiveChain = resolved
		}
	}
	if effectiveChain == "" {
		fmt.Fprintln(os.Stderr, "No default chain found in .contenox/ (workspace) or ~/.contenox/.")
		fmt.Fprintln(os.Stderr, "Run 'contenox init' to scaffold one, or pass --chain explicitly.")
		return errChainRequired
	}

	effectiveEnableLocalExec, _ := flags.GetBool("shell")
	effectiveLocalExecAllowedDir, _ := flags.GetString("local-exec-allowed-dir")

	effectiveTracing, _ := flags.GetBool("trace")
	effectiveSteps, _ := flags.GetBool("steps")
	effectiveRaw, _ := flags.GetBool("raw")

	var inputValue string
	var inputPassed bool
	if useEditor {
		var seed []byte
		if data, ok, err := readStdinIfAvailable(maxCLIStdinBytes); err != nil {
			return err
		} else if ok {
			seed = []byte(data)
		}
		prompt, err := captureFromEditor(seed, effectiveModel)
		if err != nil {
			if errors.Is(err, errEmptyPrompt) {
				fmt.Fprintln(cmd.ErrOrStderr(), "aborted due to empty prompt")
				return errPromptAborted
			}
			return err
		}
		inputValue = prompt
		inputPassed = true
	} else if changed("input") {
		rawInput, _ := flags.GetString("input")
		inputValue, err = resolveInputFlagValue("--input", rawInput)
		if err != nil {
			return err
		}
		inputPassed = true
	} else if len(args) > 0 {
		inputValue = strings.Join(args, " ")
	}

	timeout, _ := flags.GetDuration("timeout")
	timeoutCtx, timeoutCancel := context.WithTimeout(libtracker.WithNewRequestID(context.Background()), timeout)
	defer timeoutCancel()

	// Use signal.NotifyContext so cleanup is automatic when the cmd returns;
	// avoids leaking a goroutine blocked forever on <-sigCh.
	ctx, stop := signal.NotifyContext(timeoutCtx, syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	effectiveThink, err := resolveEffectiveThink(dbCtx, store, flags)
	if err != nil {
		return err
	}
	autoMode, _ := cmd.Flags().GetBool("auto")
	effectiveHITL := !autoMode
	historyTrim, _ := cmd.Flags().GetInt("trim")
	lastN, _ := cmd.Flags().GetInt("last")
	attachPaths, _ := cmd.Flags().GetStringArray("attach")

	opts := chatOpts{
		EffectiveDB:                  dbPath,
		EffectiveChain:               effectiveChain,
		EffectiveDefaultModel:        effectiveModel,
		EffectiveDefaultProvider:     effectiveDefaultProvider,
		EffectiveConfiguredModel:     configuredDefaultModel,
		EffectiveConfiguredProvider:  configuredDefaultProvider,
		EffectiveAltDefaultModel:     effectiveAltModel,
		EffectiveAltDefaultProvider:  effectiveAltProvider,
		EffectiveMaxTokens:           effectiveMaxTokens,
		EffectiveContext:             effectiveContext,
		EffectiveNoDeleteModels:      effectiveNoDeleteModels,
		EffectiveEnableLocalExec:     effectiveEnableLocalExec,
		EffectiveLocalExecAllowedDir: effectiveLocalExecAllowedDir,
		EffectiveTracing:             effectiveTracing,
		EffectiveSteps:               effectiveSteps,
		EffectiveHITL:                effectiveHITL,
		EffectiveRaw:                 effectiveRaw,
		EffectiveThink:               effectiveThink,
		HistoryTrim:                  historyTrim,
		LastN:                        lastN,
		InputValue:                   inputValue,
		InputFlagPassed:              inputPassed,
		AttachPaths:                  attachPaths,
		ContenoxDir:                  contenoxDir,
	}
	return execChat(ctx, db, opts, cmd.OutOrStdout(), cmd.ErrOrStderr())
}

func resolveEffectiveThink(ctx context.Context, store runtimetypes.Store, flags *pflag.FlagSet) (string, error) {
	if flags != nil && flags.Changed("think") {
		v, _ := flags.GetString("think")
		level, err := reasoning.Normalize(v)
		if err != nil {
			return "", fmt.Errorf("--think: %w", err)
		}
		return level, nil
	}
	if store != nil {
		if kv, _ := getConfigKV(ctx, store, "default-think"); kv != "" {
			level, err := reasoning.Normalize(kv)
			if err != nil {
				return "", fmt.Errorf("config default-think: %w", err)
			}
			return level, nil
		}
	}
	return reasoning.Default, nil
}

func resolveEffectiveMaxTokens(ctx context.Context, store runtimetypes.Store, flags *pflag.FlagSet) (string, error) {
	if flags != nil && flags.Changed("max-tokens") {
		n, _ := flags.GetInt("max-tokens")
		if n < 0 {
			return "", fmt.Errorf("--max-tokens must be non-negative, got %d", n)
		}
		return strconv.Itoa(n), nil
	}
	if store != nil {
		if kv, _ := getConfigKV(ctx, store, "default-max-tokens"); kv != "" {
			return normalizeMaxTokensConfig(kv)
		}
	}
	return "", nil
}

func shouldPrintThinking(level string) bool {
	return reasoning.DisplayEnabled(level)
}

// Sentinel errors so RunE can return and main can os.Exit(1).
var (
	errChainRequired = &exitError{1}
	errPromptAborted = &exitError{1}
)

type exitError struct{ code int }

func (e *exitError) Error() string { return "exit" }

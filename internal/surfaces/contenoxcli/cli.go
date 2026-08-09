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

	"github.com/contenox/contenox/internal/kernel/reasoning"
	"github.com/contenox/contenox/internal/models/modelrepo"
	"github.com/contenox/contenox/internal/services/clikv"
	"github.com/contenox/contenox/internal/services/eventlog"
	"github.com/contenox/contenox/internal/services/project"
	"github.com/contenox/contenox/internal/store/runtimetypes"
	"github.com/contenox/contenox/internal/version"
	"github.com/contenox/contenox/libtracker"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// Version is an optional link-time override via
// -ldflags "-X github.com/contenox/contenox/internal/surfaces/contenoxcli.Version=…"
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
	defaultModel   = "qwen3:8b"
	defaultContext = 0
	// defaultTimeout is a total wall clock over an entire one-shot run, not a
	// per-call bound: at 5m any `contenox "..."` or `contenox run` turn longer
	// than five minutes was cancelled outright and reported as cancelled by the
	// user. The interactive surfaces (beam, acp) never applied it, so the same
	// turn survived in the TUI and died from the CLI. Sized above the turn
	// deadline that is meant to be the real ceiling; --timeout still narrows it.
	defaultTimeout = 2 * time.Hour
)

// reservedSubcommands are first-arg names never treated as run input. Retired
// names stay reserved so typing one gets an unknown-command error rather than
// being injected as a chat prompt.
var reservedSubcommands = map[string]bool{"init": true, "chat": true, "help": true, "completion": true, "session": true, "run": true, "tools": true, "mcp": true, "backend": true, "agent": true, "config": true, "model": true, "models": true, "doctor": true, "version": true, "state": true, "acp": true, "acpx": true, "setup": true, "cache": true, "update": true, "workspace": true, "sandbox": true, "shell-env": true, "vet": true, "serve": true, "fleet": true, "mission": true, "approvals": true, "inbox": true, "code": true, "vscode-agent": true, "modeld": true, "beam": true, "new": true, "resume": true, "index": true, "search": true, "events": true, "hitl": true, "login": true, "logout": true,
	// cobra's shell-completion protocol: every TAB press invokes these; treated
	// as chat input they would run a live model call per keystroke.
	"__complete": true, "__completeNoDesc": true}

// Main runs the contenox CLI: init subcommand or run (default) with optional positional input.
func Main() {
	args := os.Args[1:]
	// Only inject "run" when no reserved subcommand was given, and not when
	// args are help/version-only.
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
	// The beta agent-roster and event-dispatch surfaces are absent from help
	// without the opt-in; Hidden gates visibility only, never execution.
	betaHidden := !betaEnabledGlobal()
	agentCmd.Hidden = betaHidden
	eventsCmd.Hidden = betaHidden
	// Beta flags on stable commands are absent, not hidden: an unregistered
	// --as-agent or --oracle neither shows in help nor parses.
	if !betaHidden {
		registerApprovalsRespondFlags(true)
		registerMissionFireFlags(true)
	}
	// Seeded with the inherited event hop so a CLI spawned by a fired chain can
	// forward it to its own spawns: the publisher reads the env var directly,
	// but only a context carries it onward (see eventlog.InheritHop).
	err := rootCmd.ExecuteContext(eventlog.InheritHop(context.Background()))
	// Flush warm-session KV snapshots before exit so the next start restores
	// warm instead of cold-prefilling. Best-effort.
	_ = modelrepo.Shutdown()
	if err != nil {
		recordStartupFailure(err)
		// A command with its own exit status (*exitError) has already printed
		// what it wanted the operator to see, so skip the generic "Error:"
		// prefix — a deliberate non-zero exit reads as a status, not a crash.
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

// dispatchSubcommand decides which subcommand a bare invocation is routed to:
// bare input is session-backed chat; 'contenox run' is the explicit stateless
// path. Returns "" when args already name a subcommand or are help/version-only.
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
	tr := libtracker.NewTextActivityTracker(f)
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
	Short: "Fire coding work at an agent, under rules you can read — chat, chains, and missions from your terminal.",
	Long: `Contenox is an open agentic harness. Chat and shell in your terminal, use
the same harness from any ACP editor, and package repeatable work into chains —
prompts, model routing, tools, retries, and approval gates in one versioned
file. State lives in local SQLite. Hosted providers and Ollama work out of the
box; for local inference run Ollama or vLLM.

  Quickstart (in this order):
    contenox setup                         # 1. start here: wizard — pick provider, model, API key
    contenox doctor                        # 2. verdict: can I chat right now, yes or no
    contenox init                          # 3. once per project: scaffold .contenox/ and its chains
    contenox new                           # 4. the terminal UI: chat, plan, and shell in one session
    contenox resume                        #    reopen your last session, transcript and all
    contenox "list files in my home dir"   #    or one-shot, session-backed chat

  Inspect models:
    contenox model list                    # models exposed by registered live backends

  Or register an LLM backend manually:
    # Local Ollama daemon
    ollama serve && ollama pull qwen3:8b
    contenox backend add ollama --type ollama
    contenox config set default-provider ollama
    contenox config set default-model qwen3:8b

    # Hosted Ollama Cloud
    contenox backend add ollama-cloud --type ollama --url https://ollama.com/api --api-key-env OLLAMA_API_KEY
    contenox config set default-provider ollama

    # Google Gemini (no GPU required)
    contenox backend add gemini --type gemini --api-key-env GEMINI_API_KEY
    contenox config set default-model gemini-flash-latest
    contenox config set default-provider gemini

    # OpenAI
    contenox backend add openai --type openai --api-key-env OPENAI_API_KEY
    contenox config set default-model gpt-5-mini
    contenox config set default-provider openai

  Editor autocomplete (FIM, over ACP) can use a separate model from chat:
    # Example: chat on OpenAI, ghost text on local Ollama.
    contenox config set default-provider openai
    contenox config set default-model gpt-5-mini
    contenox config set default-autocomplete-provider ollama
    contenox config set default-autocomplete-model qwen2.5-coder:7b

  Scope note:
    Backends and config are GLOBAL (stored in ~/.contenox/local.db).
    Chain files and HITL policy presets are GLOBAL too: 'contenox init' writes
    them to ~/.contenox/ and creates a per-project workspace marker
    (.contenox/workspace.id). Run 'contenox init' once per project for the
    marker; run 'contenox init --local' to seed workspace-local copies in
    .contenox/ that override the global files by name.`,
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
	Short: "Seed the default chain files and HITL policy presets; mark the project.",
	Long: `Seed the default chain files and HITL policy presets, and mark the project.

By default the chain files (chain-agent-contenox.json, chain-agent-run.json, …) and
the hitl-policy-*.json presets are written to ~/.contenox/ and shared by every
project; the project itself only gets a workspace marker (.contenox/workspace.id).
With --local they are written into the workspace .contenox/ instead, creating
deliberate workspace-local overrides — a same-named workspace file wins over
the global copy.

'contenox setup' is the recommended entry point and runs first: it picks a provider
and model and registers the backend. Run init afterwards, once per project.

To configure by hand instead, register a backend, make sure the runtime can see a
model, then set your defaults:

  # Local Ollama:
  contenox backend add ollama --type ollama
  contenox config set default-provider ollama
  contenox config set default-model qwen3:8b

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
  contenox config set default-model gemini-flash-latest

  # Optional editor autocomplete model, independent from chat:
  contenox config set default-autocomplete-provider ollama
  contenox config set default-autocomplete-model qwen2.5-coder:7b

Use --force to overwrite existing files, or --update to refresh unchanged default files to the
latest version. --update also renames shipped chain files still carrying a pre-v0.38 name (for
example default-acp-chain.json) to the chain-<role>-<variant>.json convention, byte-for-byte —
in ~/.contenox and the workspace .contenox both — before refreshing; hand-edited files keep
their content under the new name.

Use --refresh-policies to rewrite ONLY the HITL policy presets (hitl-policy-*.json) from
this build — in ~/.contenox and in any workspace .contenox copy that shadows it. That is
what 'contenox doctor' points at when an envelope predates a shipped toolset: it leaves
chains, config and sessions alone, unlike --force.`,
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
	_ = f.MarkHidden("no-delete-models")
	f.String("chain", "", "Path to a task chain JSON file. Chains define the LLM workflow: which model, which tools, how to branch. Falls back to default-chain in config, then .contenox/chain-agent-contenox.json")
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
	rootCmd.AddCommand(approvalsCmd)
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
	rootCmd.AddCommand(eventsCmd)
	rootCmd.AddCommand(hitlCmd)

	rootCmd.InitDefaultHelpCmd() // so "contenox help" is handled by Cobra, not passed as run input
	initCmd.Flags().BoolP("force", "f", false, "Overwrite existing files")
	initCmd.Flags().Bool("update", false, "Update unchanged default files to the latest version; also renames shipped chain files still under a pre-v0.38 name to the chain-<role>-<variant>.json convention (content kept byte-for-byte)")
	initCmd.Flags().Bool("refresh-policies", false, "Rewrite ONLY the HITL policy presets from this build, in ~/.contenox and any workspace .contenox copy that shadows it (chains, config and sessions are untouched; your edits to those policy files are replaced)")
	initCmd.Flags().Bool("local", false, "Write the chain files and HITL policy presets into the workspace .contenox/ instead of ~/.contenox — same-named workspace copies override the global ones")
	initCmd.Flags().Bool("project", false, "Create a project marker in the CURRENT directory (a fresh workspace id), instead of reusing an ancestor's .contenox")
	initCmd.Flags().String("name", "", "Friendly project name for the marker (default: the directory name)")

	// Chat-specific local flags (not exposed globally).
	chatCmd.Flags().Int("trim", 0, "Only send the last N messages from session history to the model (0 = send all)")
	chatCmd.Flags().StringArray("attach", nil, "Attach an image to this message (repeatable). Routes to a vision-capable model.")
	chatCmd.Flags().Int("last", 0, "Print last N user/assistant turns after the reply (0 = only print new reply)")
	chatCmd.Flags().Bool("auto", false, "Non-interactive mode: disable HITL approval prompts. Default is HITL on; tools route through the active hitl-policy. Use --auto only in trusted/scripted contexts.")

}

// setupTelemetryLogging points slog at both stderr and telemetry.log when the
// operator has enabled it, returning a cleanup func. Sink wiring only: nothing
// else in this package's command logic may call slog as an API.
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

// warnTelemetryLoggingUnavailable reports a failed setupTelemetryLogging on
// the command's own stderr, not through the tracker — whose sink is the
// logging that just failed to come up.
func warnTelemetryLoggingUnavailable(w io.Writer, err error) {
	fmt.Fprintf(w, "warning: telemetry-enabled is set but its log file could not be opened, continuing without it: %v\n"+
		"         turn it off with: contenox config set telemetry-enabled false\n", err)
}

// ResolveContenoxDir finds the closest .contenox directory by walking up from
// cwd, or returns --data-dir directly when set. Falls back to cwd/.contenox
// if it hits the root without finding one.
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
			// A .contenox/ without workspace.id isn't a valid workspace.
			if _, werr := os.Stat(filepath.Join(candidate, "workspace.id")); werr == nil {
				return candidate, nil
			}
		}

		parent := filepath.Dir(dir)
		if parent == dir {
			return filepath.Join(cwd, ".contenox"), nil
		}
		dir = parent
	}
}

// controlPlaneDirs is the single definition of the runtime's control plane for
// a command: contenoxDir plus the home global ~/.contenox. No session, browse
// root, or agent fs tool may reach these (runtime/vfs/controlplane.go); serve
// and the `workspace add` grant guard both derive their denylist from here so
// the two never disagree. A database relocated by --db is not auto-denied:
// denying its parent could swallow a legitimate workspace root.
func controlPlaneDirs(contenoxDir string) []string {
	dirs := []string{contenoxDir}
	if home, err := globalContenoxDir(); err == nil {
		dirs = append(dirs, home)
	}
	return dirs
}

func ResolveWorkspaceID(contenoxDir string) string {
	// project owns the marker format so serve, the CLI, and the API agree.
	if m, ok := project.ReadFromContenoxDir(contenoxDir); ok && m.ID != "" {
		return m.ID
	}
	return DefaultWorkspaceID
}

func runInitCmd(cmd *cobra.Command, args []string) error {
	// Narrower than --force: presets only, no chains, marker, or config. The
	// policy loader is workspace-first, so the refresh needs the resolved
	// workspace dir, not just ~/.contenox.
	if refresh, _ := cmd.Flags().GetBool("refresh-policies"); refresh {
		contenoxDir, err := ResolveContenoxDir(cmd)
		if err != nil {
			return fmt.Errorf("failed to resolve .contenox dir: %w", err)
		}
		return runRefreshPolicies(cmd.OutOrStdout(), contenoxDir, betaGatedToolsets(betaEnabledGlobal()))
	}
	force, _ := cmd.Flags().GetBool("force")
	update, _ := cmd.Flags().GetBool("update")
	localMode, _ := cmd.Flags().GetBool("local")
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
		// A local project marker in cwd, bypassing the ancestor walk-up.
		cwd, err := os.Getwd()
		if err != nil {
			return fmt.Errorf("failed to resolve current directory: %w", err)
		}
		contenoxDir, projectName = resolveProjectInit(cwd, projectName)
		if localMode {
			if err := RunLocalInit(cmd.OutOrStdout(), force, update, contenoxDir, projectName); err != nil {
				return err
			}
		} else if err := RunInit(cmd.OutOrStdout(), cmd.ErrOrStderr(), force, update, provider, contenoxDir, projectName); err != nil {
			return err
		}
		// Marking a project doesn't grant it as a workspace root; that's a
		// separate decision, made with this verb.
		fmt.Fprintf(cmd.OutOrStdout(),
			"To let sessions open it (the beam picker): contenox workspace add %s\n", cwd)
		return nil
	}
	contenoxDir, err = ResolveContenoxDir(cmd)
	if err != nil {
		return fmt.Errorf("failed to resolve .contenox dir: %w", err)
	}
	if localMode {
		return RunLocalInit(cmd.OutOrStdout(), force, update, contenoxDir, projectName)
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
		warnTelemetryLoggingUnavailable(cmd.ErrOrStderr(), err)
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
		// Workspace-scoped: read at the same scope `contenox config set
		// default-chain` writes, not the global row alone.
		if kv, _ := clikv.ReadConfig(dbCtx, store, ResolveWorkspaceID(contenoxDir), clikv.KeyDefaultChain); kv != "" {
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
		if resolved, rerr := lookupSystemFile(contenoxDir, chainAgentContenoxFilename); rerr == nil {
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
		EffectiveOptInBeta:           betaEnabled(dbCtx, store),
		HistoryTrim:                  historyTrim,
		LastN:                        lastN,
		InputValue:                   inputValue,
		InputFlagPassed:              inputPassed,
		AttachPaths:                  attachPaths,
		ContenoxDir:                  contenoxDir,
		WarnW:                        cmd.ErrOrStderr(),
		// A terminal gets the answer as it is produced; a pipe keeps the single
		// buffered payload its consumer parses.
		EffectiveStreamOutput: stdoutIsTerminal(),
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

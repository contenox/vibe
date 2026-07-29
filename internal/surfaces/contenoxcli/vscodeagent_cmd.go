package contenoxcli

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	libdb "github.com/contenox/contenox/internal/libdbexec"
	"github.com/contenox/contenox/internal/libkvstore"
	"github.com/contenox/contenox/internal/libtracker"
	"github.com/contenox/contenox/internal/models/modelrepo"
	"github.com/contenox/contenox/internal/services/agentservice"
	"github.com/contenox/contenox/internal/services/presence"
	"github.com/contenox/contenox/internal/store/runtimetypes"
	"github.com/contenox/contenox/internal/surfaces/vscodeagent"
	"github.com/spf13/cobra"
)

var vscodeAgentCmd = &cobra.Command{
	Use:   "vscode-agent",
	Short: "Run the Contenox VS Code bridge over stdio.",
	Long: `Speak the narrow Contenox VS Code bridge protocol over stdio.

The VS Code extension owns editor APIs and starts this process. The Go bridge
owns Contenox state, provider/model configuration, chat, autocomplete, sessions,
and HITL approval routing. Stdout is reserved for framed JSON-RPC. Logs and
diagnostics go to stderr.`,
	RunE: runVSCodeAgent,
}

func init() {
	vscodeAgentCmd.Flags().Bool("stdio", true, "Serve the VS Code bridge over stdin/stdout")
	vscodeAgentCmd.Flags().String("workspace-id", "", "Workspace ID for workspace-scoped configuration (default: ~/.contenox/workspace.id or the CLI fallback)")
	vscodeAgentCmd.Flags().Bool("auto", false, "Non-interactive mode: disable HITL approval prompts. Default is HITL on.")
}

func runVSCodeAgent(cmd *cobra.Command, _ []string) error {
	stdio, _ := cmd.Flags().GetBool("stdio")
	if !stdio {
		return fmt.Errorf("vscode-agent currently only supports --stdio")
	}

	parentCtx := cmd.Context()
	if parentCtx == nil {
		parentCtx = context.Background()
	}
	ctx, stop := signal.NotifyContext(parentCtx, syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	// Deferred before the runtime is built so it runs after runtime teardown
	// (LIFO): drain registered model-backend shutdown hooks (e.g. native
	// in-process inference sessions) deterministically on exit. No-op when no
	// hook is registered.
	defer func() { _ = modelrepo.Shutdown() }()

	trace, _ := cmd.Flags().GetBool("trace")

	var tracker libtracker.ActivityTracker = libtracker.NoopTracker{}
	if trace {
		// nil logger = the process default handler; slog is the tracker's sink,
		// not an API this file calls.
		tracker = libtracker.NewLogActivityTracker(nil)
	}

	reportErr, reportChange, end := tracker.Start(ctx, "startup", "vscode_agent")
	defer end()

	contenoxDir, err := resolveVSCodeAgentContenoxDir(cmd)
	if err != nil {
		reportErr(err)
		return err
	}
	reportChange("phase", "resolve_contenox_dir")

	dbPath, err := resolveVSCodeAgentDBPath(cmd, contenoxDir)
	if err != nil {
		reportErr(err)
		return err
	}
	db, err := OpenDBAt(libtracker.WithNewRequestID(ctx), dbPath)
	if err != nil {
		reportErr(err)
		return fmt.Errorf("open database %q: %w", dbPath, err)
	}
	defer db.Close()
	reportChange("phase", "open_db")

	if err := writeEmbeddedHITLPolicies(contenoxDir, false); err != nil {
		reportErr(err)
		return fmt.Errorf("seed HITL policy presets: %w", err)
	}
	reportChange("phase", "seed_hitl")

	if err := seedVSCodeAgentChainsIfMissing(contenoxDir); err != nil {
		reportErr(err)
		return fmt.Errorf("seed VS Code chain presets: %w", err)
	}
	reportChange("phase", "seed_chains")

	workspaceID, _ := cmd.Flags().GetString("workspace-id")
	if workspaceID == "" {
		workspaceID = ResolveWorkspaceID(contenoxDir)
	}
	workspaceCWD, _ := os.Getwd()
	reportChange("phase", "prep_done")

	// Fleet presence: make this VS Code bridge process visible on the fleet board.
	// It self-registers into the shared-SQLite presence store (the same $HOME/
	// .contenox/local.db serve reads) and heartbeats on a modest interval. The
	// kind (vscode-agent) is the client identity here — VS Code is the only thing
	// that runs this bridge — so no separate client-name capture is wired. Fully
	// best-effort: a presence write never blocks or fails the bridge (see
	// internal/services/presence).
	presenceReporter := presence.StartReporter(ctx, presence.NewStore(libkvstore.NewSQLiteManager(db)), presence.Record{
		Kind: presence.KindVSCodeAgent,
		Cwd:  workspaceCWD,
	})
	defer presenceReporter.Stop()

	server, err := vscodeagent.New(vscodeagent.ServerConfig{
		DB:           db,
		StateDir:     contenoxDir,
		WorkspaceID:  workspaceID,
		WorkspaceCWD: workspaceCWD,
		Version:      CLIVersion(),
		RuntimeBuilder: func(buildCtx context.Context, hooks vscodeagent.RuntimeHooks) (*vscodeagent.Runtime, error) {
			return buildVSCodeAgentRuntime(buildCtx, cmd, db, contenoxDir, workspaceID, hooks)
		},
		PolicyNames: embeddedPolicyNames(),
	})
	if err != nil {
		reportErr(err)
		return err
	}
	reportChange("phase", "server_created")

	if err := server.Run(ctx, os.Stdin, os.Stdout); err != nil && err != io.EOF && err != context.Canceled {
		reportErr(err)
		return fmt.Errorf("vscode-agent run: %w", err)
	}
	return nil
}

func buildVSCodeAgentRuntime(ctx context.Context, cmd *cobra.Command, db libdb.DBManager, contenoxDir, workspaceID string, hooks vscodeagent.RuntimeHooks) (*vscodeagent.Runtime, error) {
	store := runtimetypes.New(db.WithoutTransaction())
	cfgCtx := libtracker.WithNewRequestID(ctx)
	flags := cmd.Root().Flags()

	trace, _ := flags.GetBool("trace")
	var buildTracker libtracker.ActivityTracker = libtracker.NoopTracker{}
	if trace {
		buildTracker = libtracker.NewLogActivityTracker(nil)
	}

	reportErr, reportChange, end := buildTracker.Start(ctx, "build", "vscode_agent_runtime")
	defer end()

	model, _ := flags.GetString("model")
	if !flags.Changed("model") || model == defaultModel {
		if kv, _ := getConfigKV(cfgCtx, store, "default-model"); kv != "" {
			model = kv
		}
	}
	provider := ""
	if kv, _ := getConfigKV(cfgCtx, store, "default-provider"); kv != "" {
		provider = kv
	}
	if flags.Changed("provider") {
		provider, _ = flags.GetString("provider")
	}
	if model == "" || provider == "" {
		err := vscodeagent.ErrSetupRequired
		reportErr(err)
		return nil, err
	}
	reportChange("phase", "resolve_defaults")

	altModel := ""
	if kv, _ := getConfigKV(cfgCtx, store, "default-alt-model"); kv != "" {
		altModel = kv
	}
	if flags.Changed("alt-model") {
		altModel, _ = flags.GetString("alt-model")
	}
	altProvider := ""
	if kv, _ := getConfigKV(cfgCtx, store, "default-alt-provider"); kv != "" {
		altProvider = kv
	}
	if flags.Changed("alt-provider") {
		altProvider, _ = flags.GetString("alt-provider")
	}
	maxTokens, err := resolveEffectiveMaxTokens(cfgCtx, store, flags)
	if err != nil {
		reportErr(err)
		return nil, err
	}
	reportChange("phase", "resolve_max_tokens")
	think, err := resolveEffectiveThink(cfgCtx, store, flags)
	if err != nil {
		reportErr(err)
		return nil, err
	}
	reportChange("phase", "resolve_think")
	contextLength, _ := flags.GetInt("context")
	noDeleteModels, _ := flags.GetBool("no-delete-models")
	shellEnabled := true
	if flags.Changed("shell") {
		shellEnabled, _ = flags.GetBool("shell")
	}
	allowedDir, _ := flags.GetString("local-exec-allowed-dir")
	if allowedDir == "" {
		if cwd, err := os.Getwd(); err == nil {
			allowedDir = cwd
		}
	}
	if allowedDir != "" {
		if abs, err := filepath.Abs(allowedDir); err == nil {
			allowedDir = abs
		}
	}
	auto, _ := cmd.Flags().GetBool("auto")

	chainPath, err := resolveVSCodeAgentChainPath(cmd, contenoxDir, "default-acp-chain.json")
	if err != nil {
		reportErr(err)
		return nil, err
	}
	reportChange("phase", "resolve_acp_chain_path")
	chain, err := loadChainFromFile(chainPath)
	if err != nil {
		reportErr(err)
		return nil, err
	}
	reportChange("phase", "load_acp_chain")
	fimPath, err := resolveVSCodeAgentChainPath(cmd, contenoxDir, "default-fim-chain.json")
	if err != nil {
		reportErr(err)
		return nil, err
	}
	reportChange("phase", "resolve_fim_chain_path")
	fimChain, err := loadChainFromFile(fimPath)
	if err != nil {
		reportErr(err)
		return nil, err
	}
	reportChange("phase", "load_fim_chain")
	compactPath, err := resolveVSCodeAgentChainPath(cmd, contenoxDir, "chain-compact.json")
	if err != nil {
		reportErr(err)
		return nil, err
	}
	reportChange("phase", "resolve_compact_chain_path")
	compactChain, err := loadChainFromFile(compactPath)
	if err != nil {
		reportErr(err)
		return nil, err
	}
	reportChange("phase", "load_compact_chain")

	opts := chatOpts{
		EffectiveDefaultModel:        model,
		EffectiveDefaultProvider:     provider,
		EffectiveAltDefaultModel:     altModel,
		EffectiveAltDefaultProvider:  altProvider,
		EffectiveMaxTokens:           maxTokens,
		EffectiveContext:             contextLength,
		EffectiveNoDeleteModels:      noDeleteModels,
		EffectiveEnableLocalExec:     shellEnabled,
		EffectiveLocalExecAllowedDir: allowedDir,
		EffectiveTracing:             trace,
		EffectiveHITL:                !auto,
		EffectiveThink:               think,
		EffectiveAskApproval:         hooks.AskApproval,
		EffectiveTaskEventSink:       hooks.EventSink,
		ContenoxDir:                  contenoxDir,
	}
	reportChange("phase", "opts_prepared")
	engine, err := BuildEngine(ctx, db, opts)
	if err != nil {
		reportErr(err)
		return nil, fmt.Errorf("build engine: %w", err)
	}
	reportChange("phase", "engine_built")
	agent := agentservice.New(agentservice.Deps{
		Engine:      engine,
		DB:          db,
		WorkspaceID: workspaceID,
		Identity:    vscodeagent.Identity,
	})
	reportChange("phase", "runtime_ready")
	return &vscodeagent.Runtime{
		Engine:       engine,
		Agent:        agent,
		Chain:        chain,
		FIMChain:     fimChain,
		CompactChain: compactChain,
		Close:        engine.Stop,
	}, nil
}

func resolveVSCodeAgentContenoxDir(cmd *cobra.Command) (string, error) {
	dataDir, _ := cmd.Root().PersistentFlags().GetString("data-dir")
	if dataDir == "" {
		return globalContenoxDir()
	}
	abs, err := filepath.Abs(dataDir)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(abs, 0o700); err != nil {
		return "", fmt.Errorf("create data dir %q: %w", abs, err)
	}
	return abs, nil
}

func resolveVSCodeAgentDBPath(cmd *cobra.Command, contenoxDir string) (string, error) {
	dbFlag, _ := cmd.Flags().GetString("db")
	if dbFlag == "" {
		dbFlag, _ = cmd.Root().PersistentFlags().GetString("db")
	}
	if dbFlag != "" {
		return filepath.Abs(dbFlag)
	}
	return filepath.Join(contenoxDir, "local.db"), nil
}

func seedVSCodeAgentChainsIfMissing(contenoxDir string) error {
	if err := seedACPChainIfMissing(contenoxDir); err != nil {
		return err
	}
	if err := writeVSCodeAgentChainIfMissing(contenoxDir, "chain-compact.json", initCompactChain); err != nil {
		return err
	}
	return writeVSCodeAgentChainIfMissing(contenoxDir, "default-fim-chain.json", initFIMChain)
}

func writeVSCodeAgentChainIfMissing(contenoxDir, name, body string) error {
	dst := filepath.Join(contenoxDir, name)
	if _, err := os.Stat(dst); err == nil {
		return nil
	}
	if err := os.MkdirAll(contenoxDir, 0750); err != nil {
		return err
	}
	return os.WriteFile(dst, []byte(body), 0644)
}

func resolveVSCodeAgentChainPath(cmd *cobra.Command, contenoxDir, fallbackName string) (string, error) {
	if fallbackName == "default-acp-chain.json" {
		if chainFlag, _ := cmd.Root().PersistentFlags().GetString("chain"); chainFlag != "" {
			return filepath.Abs(chainFlag)
		}
	}
	path := filepath.Join(contenoxDir, fallbackName)
	if _, err := os.Stat(path); err == nil {
		return path, nil
	}
	return lookupSystemFile(contenoxDir, fallbackName)
}

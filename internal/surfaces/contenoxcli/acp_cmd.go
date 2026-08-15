package contenoxcli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/contenox/contenox/internal/kernel/enginesvc"
	"github.com/contenox/contenox/internal/kernel/reasoning"
	"github.com/contenox/contenox/internal/kernel/taskengine"
	"github.com/contenox/contenox/internal/models/modelrepo"
	"github.com/contenox/contenox/internal/services/agentregistryservice"
	"github.com/contenox/contenox/internal/services/agentservice"
	"github.com/contenox/contenox/internal/services/eventlog"
	"github.com/contenox/contenox/internal/services/fleetservice"
	"github.com/contenox/contenox/internal/services/gojatool"
	"github.com/contenox/contenox/internal/services/hitlservice"
	"github.com/contenox/contenox/internal/services/localtools"
	"github.com/contenox/contenox/internal/services/missionservice"
	"github.com/contenox/contenox/internal/services/oracletools"
	"github.com/contenox/contenox/internal/services/presence"
	"github.com/contenox/contenox/internal/services/updatecheck"
	"github.com/contenox/contenox/internal/store/runtimetypes"
	"github.com/contenox/contenox/internal/surfaces/acpsvc"
	"github.com/contenox/contenox/internal/surfaces/fleetboot"
	"github.com/contenox/contenox/libacp"
	"github.com/contenox/contenox/libbus"
	libdb "github.com/contenox/contenox/libdbexec"
	"github.com/contenox/contenox/libkvstore"
	"github.com/contenox/contenox/libtracker"
	"github.com/spf13/cobra"
)

var acpCmd = &cobra.Command{
	Use:   "acp",
	Short: "Run the Contenox ACP server over stdio.",
	Long: `Speak Agent Client Protocol over stdio so editors like Zed can run local Contenox chains.

The chain executed for each session/prompt is loaded from ~/.contenox/chain-agent-acp.json
(override with the CONTENOX_ACP_CHAIN_PATH environment variable). Populate it like any other
contenox chain.

The default model is read from the global 'default-model' / 'default-provider' configuration
(set via 'contenox config set default-model …'). Logging goes to stderr; stdin/stdout are
reserved for the JSON-RPC stream.

HITL is on by default — gated tool calls route through the ACP session/request_permission
flow so the editor's UI controls approval. Pass --auto to disable (unattended/testing).

The /mission slash command dispatches a mission in-process: the fired unit is a child
subprocess of this editor session and its reports arrive live back in the firing session.`,
	RunE: runACP,
}

var acpxCmd = &cobra.Command{
	Use:   "acpx",
	Short: "Run as an ACP agent under the headless / untrusted-driver profile (OpenClaw).",
	Long: `Same Agent Client Protocol server as 'acp', for drivers that are not the
device owner — OpenClaw and other non-editor clients. It loads the hardened
hitl-policy-acpx.json (local_shell denied, web mutations denied, web reads
gated) and the chain at ~/.contenox/chain-agent-acpx.json (override with
CONTENOX_ACPX_CHAIN_PATH).

Containment for the untrusted driver is the HITL policy, not an in-chain
step. IDE clients (Zed, GoLand, AionUi) should keep using 'acp'. Selection
is per-spawn: each ACP client launches its own contenox process, so the two
profiles never share state.`,
	RunE: runACPX,
}

func init() {
	for _, c := range []*cobra.Command{acpCmd, acpxCmd} {
		c.Flags().Bool("auto", false, "Non-interactive mode: disable HITL permission prompts (gated tools run unattended)")
		c.Flags().Bool("setup", false, "Run interactive setup wizard to configure provider and model, then exit.")
		c.Flags().String("workspace-id", "", "Workspace ID for new ACP sessions (default: the stable workspace from ~/.contenox/workspace.id, same as the CLI). Existing sessions are always located by their session ID regardless of workspace.")
		registerOracleFlags(c)
		addWorkspaceRootFlag(c)
	}
	acpCmd.Flags().Bool("experimental-acp", false, "Accepted for compatibility with ACP clients that hardcode this launch flag (e.g. AionUi); no effect.")
	_ = acpCmd.Flags().MarkHidden("experimental-acp")
}

type acpStdio struct{}

func (acpStdio) Read(p []byte) (int, error)  { return os.Stdin.Read(p) }
func (acpStdio) Write(p []byte) (int, error) { return os.Stdout.Write(p) }
func (acpStdio) Close() error                { return os.Stdin.Close() }

type acpProfile struct {
	hitlPolicy   string
	chainFile    string
	chainEnv     string
	seedChain    func(contenoxDir string) error
	embedFleet   bool
	seedFIMChain func(contenoxDir string) error
}

var acpProfileACP = acpProfile{
	hitlPolicy:   "hitl-policy-acp.json",
	chainFile:    chainAgentACPFilename,
	chainEnv:     "CONTENOX_ACP_CHAIN_PATH",
	seedChain:    seedACPChainIfMissing,
	embedFleet:   true,
	seedFIMChain: seedFIMChainIfMissing,
}

var acpProfileACPX = acpProfile{
	hitlPolicy: "hitl-policy-acpx.json",
	chainFile:  chainAgentACPXFilename,
	chainEnv:   "CONTENOX_ACPX_CHAIN_PATH",
	seedChain:  seedHeadlessACPChainIfMissing,
}

func runACP(cmd *cobra.Command, _ []string) error  { return runACPProfile(cmd, acpProfileACP) }
func runACPX(cmd *cobra.Command, _ []string) error { return runACPProfile(cmd, acpProfileACPX) }

func seedOptionalFIMChain(profile acpProfile, contenoxDir string) {
	if profile.seedFIMChain == nil {
		return
	}
	if err := profile.seedFIMChain(contenoxDir); err != nil {
		fmt.Fprintf(os.Stderr, "contenox acp: seed FIM chain preset: %v\n", err)
	}
}

func loadOptionalFIMChain(ctx context.Context, tracker libtracker.ActivityTracker, profile acpProfile) *acpsvc.ChainRegistry {
	if profile.seedFIMChain == nil {
		return nil
	}
	fim, err := acpsvc.LoadFIMChainRegistry()
	if err != nil {
		fmt.Fprintf(os.Stderr, "contenox acp: autocomplete not configured: %v\n", err)
		return nil
	}
	_, _, end := tracker.Start(ctx, "load", "fim_chain", "source", fim.Source(), "id", fim.Default().ID)
	end()
	return fim
}

func runACPProfile(cmd *cobra.Command, profile acpProfile) error {
	if setup, _ := cmd.Flags().GetBool("setup"); setup {
		return runSetup(cmd, cmd.OutOrStdout())
	}

	parentCtx := cmd.Context()
	if parentCtx == nil {
		parentCtx = context.Background()
	}
	ctx, stop := signal.NotifyContext(parentCtx, syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Deferred before the engine is built so it runs after engine teardown.
	defer func() { _ = modelrepo.Shutdown() }()

	autoMode, _ := cmd.Flags().GetBool("auto")
	enableHITL := !autoMode

	workspaceFlag, _ := cmd.Flags().GetString("workspace-id")

	var tracker libtracker.ActivityTracker = libtracker.NewTextActivityTracker(os.Stderr)

	reportErr, reportChange, endStartup := tracker.Start(ctx, "startup", "acp")
	defer endStartup()
	reportChange("phase", "flags_parsed")

	dbPath, err := resolveDBPath(cmd)
	if err != nil {
		reportErr(err)
		return err
	}
	reportChange("phase", "resolve_db_path")
	dbCtx := libtracker.WithNewRequestID(ctx)
	db, err := OpenDBAt(dbCtx, dbPath)
	if err != nil {
		reportErr(err)
		return fmt.Errorf("open database %q: %w", dbPath, err)
	}
	defer db.Close()
	reportChange("phase", "open_db")

	// Must match the directory the DB, chain and HITL policy loaders resolve to.
	contenoxDir, err := globalContenoxDir()
	if err != nil {
		reportErr(err)
		return fmt.Errorf("resolve contenox dir: %w", err)
	}
	reportChange("phase", "resolve_contenox_dir")
	workspaceID := workspaceFlag
	if workspaceID == "" {
		workspaceID = ResolveWorkspaceID(contenoxDir)
	}
	if err := writeEmbeddedHITLPolicies(contenoxDir, false); err != nil {
		reportErr(err)
		return fmt.Errorf("seed HITL policy presets: %w", err)
	}
	reportChange("phase", "seed_hitl")
	if profile.seedChain != nil {
		if err := profile.seedChain(contenoxDir); err != nil {
			reportErr(err)
			return fmt.Errorf("seed ACP chain preset: %w", err)
		}
	}
	reportChange("phase", "seed_chain")
	seedOptionalFIMChain(profile, contenoxDir)
	reportChange("phase", "seed_fim_chain")

	closeLogs, err := setupTelemetryLogging(ctx, runtimetypes.New(db.WithoutTransaction()), contenoxDir)
	if err != nil {
		reportErr(err)
		return fmt.Errorf("setup telemetry logging: %w", err)
	}
	defer closeLogs()
	reportChange("phase", "setup_telemetry")

	var transport *acpsvc.Transport

	if acpsvc.ReadConfigValue(ctx, db, "default-model") == "" &&
		(os.Getenv(envDefaultProvider) != "" || os.Getenv(envDefaultModel) != "") {
		if err := completeEnvSetup(ctx, db); err != nil {
			fmt.Fprintf(os.Stderr, "contenox acp: environment-based setup incomplete: %v\n", err)
		} else {
			fmt.Fprintln(os.Stderr, "contenox acp: configured provider/model from environment.")
		}
	}

	// Config reads are environment-first: CONTENOX_DEFAULT_* overrides the stored value without persisting.
	optInBeta := betaEnabled(ctx, runtimetypes.New(db.WithoutTransaction()))
	defaultModel := configValueWithEnv(ctx, db, "default-model", envDefaultModel)
	defaultProvider := configValueWithEnv(ctx, db, "default-provider", envDefaultProvider)
	defaultAltModel := configValueWithEnv(ctx, db, "default-alt-model", envDefaultAltModel)
	defaultAltProvider := configValueWithEnv(ctx, db, "default-alt-provider", envDefaultAltProvider)
	defaultMaxTokens, err := normalizeMaxTokensConfig(configValueWithEnv(ctx, db, "default-max-tokens", envDefaultMaxTokens))
	if err != nil {
		return err
	}
	defaultThink := reasoning.Default
	if configuredThink := configValueWithEnv(ctx, db, "default-think", envDefaultThink); configuredThink != "" {
		level, err := reasoning.Normalize(configuredThink)
		if err != nil {
			return fmt.Errorf("config default-think: %w", err)
		}
		defaultThink = level
	}

	chains, err := acpsvc.LoadChainRegistryFrom(profile.chainFile, profile.chainEnv)
	if err != nil {
		return err
	}
	_, _, end := tracker.Start(ctx, "load", "acp_chain", "source", chains.Source(), "id", chains.Default().ID)
	end()
	fimChains := loadOptionalFIMChain(ctx, tracker, profile)

	// Shared bus: the engine doesn't own it, so this defer closes it.
	bus := libbus.NewSQLite(db.WithoutTransaction())
	defer bus.Close()
	trigHook := eventlog.NewTriggerHolder()
	missionPub := missionEventPublisher(ctx, db, bus, workspaceID, tracker, trigHook)
	missions := missionservice.New(db, missionservice.WithEventPublisher(missionPub))

	acpHITL := hitlservice.NewWithDefaultPolicy(acpPolicySource(contenoxDir), runtimetypes.LocalTenantID, runtimetypes.New(db.WithoutTransaction()), tracker, profile.hitlPolicy)
	// /policy and `contenox config set hitl-policy-name` both write this workspace's row; the evaluator must read the same one.
	hitlservice.SetWorkspaceID(acpHITL, workspaceID)

	// Same SANDBOX_* scrub composition BuildEngine wires for local_shell (see sandbox_scrub.go).
	shellScrub, _, err := resolvedSandboxEnv(db, tracker, os.Stderr)
	if err != nil {
		return fmt.Errorf("resolve sandbox env: %w", err)
	}

	// Opt-in-beta gated: without it the scripts are never loaded and the provider stays unregistered.
	gojaScriptDir := filepath.Join(contenoxDir, "tools")
	if !optInBeta {
		gojaScriptDir = ""
	}
	gt, err := gojatool.New(gojatool.Config{ScriptDir: gojaScriptDir})
	if err != nil {
		return fmt.Errorf("build goja sandbox: %w", err)
	}
	toolsOwned := false
	defer func() {
		if !toolsOwned {
			gt.Shutdown()
		}
	}()

	// Declared here, assigned once the fleet is built below: the toolset's
	// subagent-start tool resolves it per call, so the ordering holds.
	var inProcessFleet fleetservice.Service

	tools := acpToolset(db, tracker, gt, workspaceID, func() *acpsvc.Transport { return transport }, shellScrub, missions, acpHITL, missionPub, optInBeta,
		func() fleetservice.Service { return inProcessFleet })

	oracleStore := runtimetypes.New(db.WithoutTransaction())
	oracleCfg := resolveOracleConfig(ctx, oracleStore, cmd)
	// A dispatched unit does not adjudicate. Its own gated calls are ruled on by
	// the host that fired it, which holds the mission envelope; mounting a second
	// oracle here makes two models judge one ask, doubling spend and leaving the
	// winner to a race — the same reason a unit embeds no fleet.
	if strings.TrimSpace(os.Getenv(profile.chainEnv)) != "" {
		oracleCfg.chain = ""
	}
	if oracleCfg.enabled() {
		tools[oracletools.ToolsProviderName] = oracletools.New(oracleResolver{
			hitl: acpHITL, missions: missions, store: oracleStore, out: os.Stderr,
		})
	}

	// One registry for every connection: without it, an approval raised by remote work would be answered on the local desk instead.
	sessionRouter := acpsvc.NewSessionRouter()

	var askApproval localtools.AskApproval
	if enableHITL {
		askApproval = routedAskApproval(sessionRouter, func() *acpsvc.Transport { return transport })
	}

	// enginesvc.Build requires a configured model; with none set, serve setup-only so "Setup Contenox" still works.
	var engine *enginesvc.Engine
	// Shared by the in-process dispatch hook and the relay chain-trigger runner so a chain fires identically either way.
	triggerOpts := chatOpts{
		EffectiveDefaultModel:       defaultModel,
		EffectiveDefaultProvider:    defaultProvider,
		EffectiveConfiguredModel:    defaultModel,
		EffectiveConfiguredProvider: defaultProvider,
		EffectiveAltDefaultModel:    defaultAltModel,
		EffectiveAltDefaultProvider: defaultAltProvider,
		EffectiveMaxTokens:          defaultMaxTokens,
		EffectiveThink:              defaultThink,
		EffectiveOptInBeta:          optInBeta,
		ContenoxDir:                 contenoxDir,
	}
	if err := acpsvc.CleanupStaleACPManagedMCPServers(ctx, db); err != nil {
		return fmt.Errorf("cleanup stale ACP MCP servers: %w", err)
	}
	if defaultModel == "" {
		fmt.Fprintln(os.Stderr, "contenox acp: no default-model configured; serving setup-only. Run the \"Setup Contenox\" auth method or `contenox acp --setup` to configure a provider and model.")
	} else {
		cfg := enginesvc.Config{
			DefaultModel:       defaultModel,
			DefaultProvider:    defaultProvider,
			AltDefaultModel:    defaultAltModel,
			AltDefaultProvider: defaultAltProvider,
			LocalTools:         tools,
			Tracker:            tracker,
			WorkspaceID:        workspaceID,
			Bus:                bus, // reuse the one bus, not a second
			// Closes the goja sandbox's construction cycle: host.tool needs the aggregate repo the sandbox is itself registered inside.
			OnToolsRepoReady: func(repo taskengine.ToolsRepo) {
				gt.SetHost(gojatool.HostFromRepo(repo))
			},
		}
		if enableHITL {
			cfg.EnableHITL = true
			cfg.AskApproval = askApproval
			cfg.HITLPolicySource = acpPolicySource(contenoxDir)
			cfg.HITLDefaultPolicyName = profile.hitlPolicy
			// The process's one durable-ask service: two instances cannot wake each other's parked waiters.
			cfg.HITLService = acpHITL
		}

		engine, err = enginesvc.Build(ctx, db, cfg)
		if err != nil {
			return fmt.Errorf("build engine: %w", err)
		}
		// Needs the engine's chat path, which now exists (same post-construction ordering as engine.go).
		bindAudioTranscriber(tools, engine)
		// Rides the engine's chainable stop so the goja sandbox joins on shutdown.
		toolsOwned = true
		oldStop := engine.Stop
		engine.Stop = func() {
			gt.Shutdown()
			oldStop()
		}
		defer engine.Stop()
		// A verdict landing with no waiter parked resumes the suspended run here.
		hitlservice.SetResumeHook(acpHITL, agentservice.ResumeHook(agentservice.Deps{
			Engine:      engine,
			DB:          db,
			WorkspaceID: workspaceID,
		}))
		if oracleCfg.enabled() {
			oracleChain, oracleChainRef, oracleErr := loadOracleChain(contenoxDir, oracleCfg)
			if oracleErr != nil {
				return oracleErr
			}
			hitlservice.SetAdjudicator(acpHITL, &oracleDriver{
				agent:         agentservice.New(agentservice.Deps{Engine: engine, DB: db, WorkspaceID: workspaceID}),
				chain:         oracleChain,
				chainRef:      oracleChainRef,
				policy:        oracleCfg.policy,
				templateVars:  buildTemplateVars(triggerOpts),
				contextLength: triggerOpts.EffectiveContext,
				approves:      oracleCfg.approves,
				missions:      missions,
				out:           os.Stderr,
			})
			fmt.Fprintln(os.Stderr, oracleMountedLine(oracleCfg))
		}
		// Live in-process trigger dispatch on this host's appends (beta; nil when no triggers load).
		trigHook.Set(buildInProcessTriggerHook(ctx, db, contenoxDir, workspaceID, engine, triggerOpts, os.Stderr))
		// Deferred AFTER engine.Stop so LIFO drains in-flight firings before teardown.
		defer trigHook.Drain(eventlog.DefaultDrainTimeout)
	}

	updateBanner := acpUpdateBanner(dbCtx, db, contenoxDir)

	presenceStore := presence.NewStore(libkvstore.NewSQLiteManager(db))

	// embedFleet gates the mission fleet to the editor profile; a dispatched unit must not host its own fleet or it would double-route its report and recursively spawn fleets.
	var (
		missionFleet      acpsvc.MissionDispatcher
		missionAgents     acpsvc.MissionAgentResolver
		stopFleetTeardown func()
	)
	isDispatchedUnit := strings.TrimSpace(os.Getenv(profile.chainEnv)) != ""
	switch {
	case !profile.embedFleet || isDispatchedUnit:
	case engine != nil:
		fleet, agents, stop, buildErr := fleetboot.BuildInProcessFleet(ctx, fleetboot.Deps{
			DB:       db,
			Bus:      bus,
			Missions: missions,
			Tracker:  tracker,
			// Late-binds this connection's live Transport (nil until the conn factory runs below).
			Transport:    func() *acpsvc.Transport { return transport },
			HITL:         acpHITL,
			PolicySource: hitlPolicySource(contenoxDir),
			DiscoverAgents: func(dctx context.Context, agents agentregistryservice.Service) {
				discoverChainAgents(dctx, agents, contenoxDir, tracker, DiscoverDeps{Store: runtimetypes.New(db.WithoutTransaction()), Bus: bus})
			},
			// The workspace missionPub stamps; a dispatched unit's own publisher must stamp the same one.
			WorkspaceID: workspaceID,
			// The database this host resolved; a dispatched unit must write its reports into the same one.
			DBPath: dbPath,
		})
		if buildErr != nil {
			return buildErr
		}
		missionFleet, missionAgents, stopFleetTeardown = fleet, agents, stop
		// Late-bound for the toolset's subagent-start tool (see acpToolset).
		inProcessFleet = fleet
	}
	if stopFleetTeardown != nil {
		// Children die with the parent: stops the report router and kills every dispatched child subprocess.
		defer stopFleetTeardown()
	}

	// Default root for a client that proposes no workspace; also bounds every relay attachment.
	launchDir, err := os.Getwd()
	if err != nil {
		reportErr(err)
		return fmt.Errorf("resolve working directory: %w", err)
	}
	workspaceRoots, err := buildWorkspaceFactory(cmd, launchDir, runtimetypes.New(db.WithoutTransaction()))
	if err != nil {
		reportErr(err)
		return err
	}

	transportFactory := acpsvc.New(acpsvc.Deps{
		Engine:             engine,
		DB:                 db,
		WorkspaceRoots:     workspaceRoots,
		ChainRegistry:      chains,
		FIMChainRegistry:   fimChains,
		DefaultModel:       defaultModel,
		DefaultProvider:    defaultProvider,
		DefaultAltModel:    defaultAltModel,
		DefaultAltProvider: defaultAltProvider,
		DefaultMaxTokens:   defaultMaxTokens,
		DefaultThink:       defaultThink,
		WorkspaceID:        workspaceID,
		ContenoxDir:        contenoxDir,
		// Every transport this factory builds registers here, so an approval goes to the connection driving the session.
		SessionRouter:         sessionRouter,
		KnownPolicies:         embeddedPolicyNames(),
		HITLDefaultPolicyName: profile.hitlPolicy,
		UpdateBanner:          updateBanner,
		// Nil-safe throughout acpsvc when unwired (acpx, a dispatched unit, or a setup-only editor).
		Fleet:  missionFleet,
		Agents: missionAgents,
		// Same values the mission toolset uses, so the command and the tool cannot disagree about who owns an ask.
		Asks: acpHITL,
		// Read from the same search path the unit's policy loader reads.
		MissionEnvelopes: newMissionEnvelopes(contenoxDir),
		OptInBeta:        optInBeta,
		EnvSetup: &acpsvc.EnvSetupSpec{
			Vars: acpEnvSetupVars(),
			Complete: func(cctx context.Context) error {
				return completeEnvSetup(cctx, db)
			},
		},
	})

	stopRelay := serveRemoteAttachments(ctx, contenoxDir, transportFactory,
		buildRelayChainTriggers(db, contenoxDir, workspaceID, engine, triggerOpts), tracker, os.Stderr)
	defer stopRelay()

	// Best-effort: never blocks or fails serving.
	acpCwd, _ := os.Getwd()
	presenceReporter := presence.StartReporter(ctx, presenceStore, presence.Record{
		Kind: presence.KindACP,
		Cwd:  acpCwd,
	})
	defer presenceReporter.Stop()

	conn := libacp.NewAgentSideConnection(acpStdio{}, func(c *libacp.AgentSideConnection) libacp.Agent {
		agent := transportFactory(c)
		transport = agent.(*acpsvc.Transport)
		return newPresenceAgent(agent, presenceReporter)
	})

	runErr := conn.Run(ctx)
	if transport != nil {
		_ = transport.Close(context.Background())
	}
	if runErr != nil && !errors.Is(runErr, io.EOF) && !errors.Is(runErr, context.Canceled) {
		return fmt.Errorf("acp run: %w", runErr)
	}
	return nil
}

// acpPolicySource is the same search path every other host uses, which is what
// puts .generated on it: a declared agent's emitted envelope lives there, and a
// source that could not read it would silently evaluate the agent under the
// built-in default instead of the envelope it was given.
func acpPolicySource(contenoxDir string) hitlservice.PolicySource {
	if contenoxDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return hitlservice.NewFSPolicySource()
		}
		contenoxDir = filepath.Join(home, ".contenox")
	}
	return hitlPolicySource(contenoxDir)
}

func acpUpdateBanner(ctx context.Context, db libdb.DBManager, contenoxDir string) string {
	if acpsvc.ReadConfigValue(ctx, db, "update-check") == "false" {
		return ""
	}

	type result struct {
		tag       string
		available bool
	}
	ch := make(chan result, 1)
	go func() {
		tag, avail, err := updatecheck.IsAvailable(ctx, CLIVersion(), contenoxDir)
		if err != nil {
			ch <- result{}
			return
		}
		ch <- result{tag, avail}
	}()

	select {
	case r := <-ch:
		if !r.available {
			return ""
		}
		return fmt.Sprintf("contenox %s is available (current: %s) — run `contenox update` to upgrade.", r.tag, CLIVersion())
	case <-time.After(500 * time.Millisecond):
		return ""
	}
}

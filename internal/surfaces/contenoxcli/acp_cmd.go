package contenoxcli

import (
	"context"
	"encoding/json"
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
	"github.com/contenox/contenox/internal/services/gointel"
	"github.com/contenox/contenox/internal/services/gojatool"
	"github.com/contenox/contenox/internal/services/hitlservice"
	"github.com/contenox/contenox/internal/services/localtools"
	"github.com/contenox/contenox/internal/services/missionservice"
	"github.com/contenox/contenox/internal/services/missiontools"
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
	}
	acpCmd.Flags().Bool("experimental-acp", false, "Accepted for compatibility with ACP clients that hardcode this launch flag (e.g. AionUi); no effect.")
	_ = acpCmd.Flags().MarkHidden("experimental-acp")
}

type acpStdio struct{}

func (acpStdio) Read(p []byte) (int, error)  { return os.Stdin.Read(p) }
func (acpStdio) Write(p []byte) (int, error) { return os.Stdout.Write(p) }
func (acpStdio) Close() error                { return os.Stdin.Close() }

type acpProfile struct {
	hitlPolicy string
	chainFile  string
	chainEnv   string
	seedChain  func(contenoxDir string) error
	// embedFleet gives this profile the `/mission` slash command, running the
	// fleet in-process. Disabled for acpx: an untrusted driver must not get a
	// lever to dispatch fleet units at all.
	embedFleet bool
	// seedFIMChain seeds the chain-fim-default.json preset consumed by
	// _contenox/autocomplete. Nil disables autocomplete for this profile
	// entirely (no seed, no load): acpx serves non-editor drivers (OpenClaw
	// and friends) with no code buffer to complete into, so the method would
	// only ever be a dead lever there. IDE clients (Zed, GoLand, AionUi) run
	// under acp and are the only consumers.
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

// seedOptionalFIMChain seeds the FIM chain preset for profiles that offer
// autocomplete. Unlike profile.seedChain (the chat chain, which fails
// startup on a seed error), this is best-effort: autocomplete is optional,
// so a seed failure (e.g. an unwritable ~/.contenox) is logged and swallowed
// rather than blocking `contenox acp`/`acpx` from serving chat.
func seedOptionalFIMChain(profile acpProfile, contenoxDir string) {
	if profile.seedFIMChain == nil {
		return
	}
	if err := profile.seedFIMChain(contenoxDir); err != nil {
		fmt.Fprintf(os.Stderr, "contenox acp: seed FIM chain preset: %v\n", err)
	}
}

// loadOptionalFIMChain resolves the _contenox/autocomplete FIM chain for
// profiles that support it, or nil for profiles that don't (acpx) or on any
// load error. Unlike acpsvc.LoadChainRegistryFrom for the chat chain (fails
// closed: a missing/invalid file is a hard error), a missing or unparseable
// FIM chain here must not break startup — Transport.handleAutocomplete
// already degrades a nil FIMChainRegistry to a clean method-not-found.
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

	// Deferred before the engine is built so it runs after engine teardown:
	// drain registered model-backend shutdown hooks. No-op if none registered.
	defer func() { _ = modelrepo.Shutdown() }()

	autoMode, _ := cmd.Flags().GetBool("auto")
	enableHITL := !autoMode

	workspaceFlag, _ := cmd.Flags().GetString("workspace-id")

	// Created early so phase timings and errors on a freeze are visible.
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

	// Anchored to $HOME/.contenox, the same directory the DB, chain and HITL
	// policy loaders resolve to — a cwd-walk here would seed presets into a
	// directory the loaders never look in.
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

	// When nothing is configured yet but the launch environment names a
	// provider/model, persist it as the interactive wizard would. Not fatal
	// on failure: falls through to setup-only mode.
	if acpsvc.ReadConfigValue(ctx, db, "default-model") == "" &&
		(os.Getenv(envDefaultProvider) != "" || os.Getenv(envDefaultModel) != "") {
		if err := completeEnvSetup(ctx, db); err != nil {
			fmt.Fprintf(os.Stderr, "contenox acp: environment-based setup incomplete: %v\n", err)
		} else {
			fmt.Fprintln(os.Stderr, "contenox acp: configured provider/model from environment.")
		}
	}

	// Config reads are environment-first: a CONTENOX_DEFAULT_* variable
	// overrides the stored value for this process without persisting.
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

	// Created before the tools and engine so both share one bus instead of
	// minting their own; the engine doesn't own it, so this defer closes it.
	bus := libbus.NewSQLite(db.WithoutTransaction())
	defer bus.Close()
	// Publisher-wired so AddReport emits a ReportAddedEvent: the dispatcher's
	// embedded report router consumes it, and a dispatched unit's publisher
	// carries its report across the process boundary the same way. Under
	// opt-in-beta the publisher also appends each event to the durable log;
	// the trigger holder fires matching triggers live once the engine exists
	// below (the standalone dispatcher stays the catch-up consumer).
	trigHook := eventlog.NewTriggerHolder()
	missionPub := missionEventPublisher(ctx, db, bus, workspaceID, tracker, trigHook)
	missions := missionservice.New(db, missionservice.WithEventPublisher(missionPub))

	// One durable-ask service for this process: the unit half raises
	// questions through it, the supervisor half answers them through it.
	acpHITL := hitlservice.NewWithDefaultPolicy(acpPolicySource(), runtimetypes.LocalTenantID, runtimetypes.New(db.WithoutTransaction()), tracker, profile.hitlPolicy)
	// /policy and `contenox config set hitl-policy-name` both write this
	// workspace's row; the evaluator must read the same one.
	hitlservice.SetWorkspaceID(acpHITL, workspaceID)

	// Same SANDBOX_* scrub composition BuildEngine wires for local_shell (see
	// sandbox_scrub.go): this profile builds its own toolset rather than going
	// through BuildEngine, so the wiring is repeated here.
	shellScrub, _, err := resolvedSandboxEnv(db, tracker, os.Stderr)
	if err != nil {
		return fmt.Errorf("resolve sandbox env: %w", err)
	}

	// The index owns a reaper goroutine, so it needs a lifecycle: it rides
	// engine.Stop below once an engine is built (toolsOwned=true), and the
	// guard here covers the setup-only path (no engine at all) and error
	// returns before that. CwdResolver, not a fixed AllowedDir: an ACP
	// session's workspace is whatever cwd the live Transport reports.
	goIndex := gointel.NewIndex(gointel.Config{
		CwdResolver: acpsvc.NewACPCwdResolver(func() *acpsvc.Transport { return transport }),
	})
	// The goja sandbox: goja_eval plus one tool per operator-authored script in
	// $CONTENOX_DIR/tools, exactly as BuildEngine constructs it for the CLI —
	// including the opt-in-beta gate: without it the scripts are never loaded
	// and acpToolset leaves the provider unregistered.
	gojaScriptDir := filepath.Join(contenoxDir, "tools")
	if !optInBeta {
		gojaScriptDir = ""
	}
	gt, err := gojatool.New(gojatool.Config{ScriptDir: gojaScriptDir})
	if err != nil {
		goIndex.Shutdown()
		return fmt.Errorf("build goja sandbox: %w", err)
	}
	toolsOwned := false
	defer func() {
		if !toolsOwned {
			goIndex.Shutdown()
			gt.Shutdown()
		}
	}()

	tools := acpToolset(db, tracker, goIndex, gt, workspaceID, func() *acpsvc.Transport { return transport }, shellScrub, missions, acpHITL, missionPub, optInBeta)

	// One registry for every connection this process serves: the stdio
	// transport built below and each remote attachment the relay tunnel
	// creates from the same factory. Without it a permission request raised by
	// work a phone started would be answered on the desk, because a single
	// engine has only ever had one place to send an approval.
	sessionRouter := acpsvc.NewSessionRouter()

	var askApproval localtools.AskApproval
	if enableHITL {
		askApproval = routedAskApproval(sessionRouter, func() *acpsvc.Transport { return transport })
	}

	// enginesvc.Build requires a configured model. When none is set, serve a
	// setup-only transport instead of hard-exiting: initialize/authenticate
	// still work, so a client can run the "Setup Contenox" auth method.
	var engine *enginesvc.Engine
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
			// Closes the goja sandbox's construction cycle: host.tool needs
			// the aggregate repo the sandbox is itself registered inside (see
			// engine.go's BuildEngine, which wires the same callback).
			OnToolsRepoReady: func(repo taskengine.ToolsRepo) {
				gt.SetHost(gojatool.HostFromRepo(repo))
			},
		}
		if enableHITL {
			cfg.EnableHITL = true
			cfg.AskApproval = askApproval
			cfg.HITLPolicySource = acpPolicySource()
			cfg.HITLDefaultPolicyName = profile.hitlPolicy
			// Inject the process's one durable-ask service: two instances
			// cannot wake each other's parked waiters.
			cfg.HITLService = acpHITL
		}

		engine, err = enginesvc.Build(ctx, db, cfg)
		if err != nil {
			return fmt.Errorf("build engine: %w", err)
		}
		// Rides the engine's chainable stop so the index's reaper and the
		// goja sandbox join on shutdown, same as engine.go's BuildEngine.
		toolsOwned = true
		oldStop := engine.Stop
		engine.Stop = func() {
			goIndex.Shutdown()
			gt.Shutdown()
			oldStop()
		}
		defer engine.Stop()
		// A verdict landing with no waiter parked (asking process died, or the
		// ask outlived its park window) resumes the suspended run here.
		hitlservice.SetResumeHook(acpHITL, agentservice.ResumeHook(agentservice.Deps{
			Engine:      engine,
			DB:          db,
			WorkspaceID: workspaceID,
		}))
		// Live in-process trigger dispatch on this host's appends (beta; nil
		// when no triggers load). Minimal opts: what buildTemplateVars and the
		// firing runner consume.
		trigHook.Set(buildInProcessTriggerHook(ctx, db, contenoxDir, workspaceID, engine, chatOpts{
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
		}, os.Stderr))
		// Deferred AFTER engine.Stop so LIFO drains in-flight firings first: an
		// editor closing the session tears this host down, and a firing killed
		// between its claim and its finish is never retried by the catch-up
		// dispatcher.
		defer trigHook.Drain(eventlog.DefaultDrainTimeout)
	}

	updateBanner := acpUpdateBanner(dbCtx, db, contenoxDir)

	// Shared-SQLite presence store: serve registers its address here, this
	// process self-registers below; board-only telemetry, harmless if unused.
	presenceStore := presence.NewStore(libkvstore.NewSQLiteManager(db))

	// The `/mission` slash command: embedFleet gates it to the editor profile
	// (acpx gets no lever), and a dispatched unit must not host its own fleet
	// or it would double-route its own report and recursively spawn fleets.
	var (
		missionFleet      acpsvc.MissionDispatcher
		missionAgents     acpsvc.MissionAgentResolver
		stopFleetTeardown func()
	)
	isDispatchedUnit := strings.TrimSpace(os.Getenv(profile.chainEnv)) != ""
	switch {
	case !profile.embedFleet || isDispatchedUnit:
		// No mission capability in this process (acpx, or this process is a unit).
	case engine != nil:
		// The default editor journey, needing a configured model since the
		// dispatched unit resolves the same $HOME state this editor runs on.
		fleet, agents, stop, buildErr := fleetboot.BuildInProcessFleet(ctx, fleetboot.Deps{
			DB:       db,
			Bus:      bus,
			Missions: missions,
			Tracker:  tracker,
			// Late-binds this connection's live Transport (nil until the conn
			// factory runs below), so the deliverer reaches the firing session.
			Transport:    func() *acpsvc.Transport { return transport },
			HITL:         acpHITL,
			PolicySource: hitlPolicySource(contenoxDir),
			DiscoverAgents: func(dctx context.Context, agents agentregistryservice.Service) {
				discoverChainAgents(dctx, agents, contenoxDir, tracker, optInBeta)
			},
			// The workspace missionPub stamps; a dispatched unit's own
			// publisher must stamp the same one.
			WorkspaceID: workspaceID,
		})
		if buildErr != nil {
			return buildErr
		}
		missionFleet, missionAgents, stopFleetTeardown = fleet, agents, stop
	}
	if stopFleetTeardown != nil {
		// Children die with the parent: stops the report router and kills
		// every dispatched child subprocess on shutdown.
		defer stopFleetTeardown()
	}

	transportFactory := acpsvc.New(acpsvc.Deps{
		Engine:             engine,
		DB:                 db,
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
		// Every transport this factory builds registers here, so an approval
		// goes to the connection driving the session rather than to the one
		// this process bound at startup.
		SessionRouter:         sessionRouter,
		KnownPolicies:         embeddedPolicyNames(),
		HITLDefaultPolicyName: profile.hitlPolicy,
		UpdateBanner:          updateBanner,
		// The in-process fleet, or nil (acpx, a dispatched unit, or a
		// setup-only editor) — nil-safe throughout acpsvc when unwired.
		Fleet:  missionFleet,
		Agents: missionAgents,
		// /answer's two seams: the durable ask store it reads and resolves
		// through, and the ownership check that confines it to asks raised by
		// missions this session fired. Same values the mission toolset uses,
		// so the command and the tool cannot disagree about who owns an ask.
		Asks:        acpHITL,
		Supervision: missionSupervision{missions: missions, hitl: acpHITL, db: db, tracker: tracker},
		// The envelopes /mission --policy offers and validates against, read
		// from the same search path the unit's policy loader reads.
		MissionEnvelopes: newMissionEnvelopes(contenoxDir),
		OptInBeta:        optInBeta,
		EnvSetup: &acpsvc.EnvSetupSpec{
			Vars: acpEnvSetupVars(),
			Complete: func(cctx context.Context) error {
				return completeEnvSetup(cctx, db)
			},
		},
	})

	stopRelay := serveRemoteAttachments(ctx, optInBeta, contenoxDir, transportFactory, tracker, os.Stderr)
	defer stopRelay()

	// Makes this process visible on the fleet board: self-registers and
	// heartbeats, entirely best-effort (never blocks or fails serving).
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

// missionAttentionAsker adapts the durable-ask machinery to
// missiontools.AttentionAsker: a unit's question becomes a pending ask, the
// wait blocks until an operator answers or the ceiling expires it, and the
// answer is returned to the unit as its tool result.
type missionAttentionAsker struct {
	hitl     hitlservice.Service
	missions missionservice.Service
	// bus is the narrow publish seam (libbus.Messenger satisfies it); wired
	// with the mission event publisher so an attention_asked announce follows
	// the same dual-write path the other mission events take.
	bus missionservice.EventPublisher
}

var _ missiontools.AttentionAsker = missionAttentionAsker{}

func (a missionAttentionAsker) RaiseAttention(ctx context.Context, ask missiontools.AttentionAsk) (string, error) {
	missionID, summary, detail := ask.MissionID, ask.Summary, ask.Detail
	// Read for attribution only, so the question is announced where the
	// operator is looking; a mission that can't be read still asks, with
	// less context.
	var parentSessionID, agentName, intent string
	if a.missions != nil {
		if m, err := a.missions.Get(ctx, missionID); err == nil && m != nil {
			parentSessionID, agentName, intent = m.ParentSessionID, m.AgentName, m.Intent
		}
	}
	answer, err := a.hitl.RequestAttention(ctx, hitlservice.AttentionRequest{
		Summary:    summary,
		Detail:     detail,
		MissionID:  missionID,
		AgentName:  agentName,
		AskID:      ask.AskID,
		ParkWindow: ask.ParkWindow,
		OnRaised: func(askID string) {
			// With no listener this changes nothing: the ask is already
			// durable and answerable from the queue.
			a.publishAsked(ctx, missionservice.AttentionAskedEvent{
				MissionID:       missionID,
				AskID:           askID,
				ParentSessionID: parentSessionID,
				AgentName:       agentName,
				Intent:          intent,
				Summary:         summary,
				Detail:          detail,
			})
		},
	}, taskengine.NoopTaskEventSink{})
	// Park window elapsed: the engine checkpoints under this ask's ID, the
	// same pattern tool-call approvals use.
	var pending *hitlservice.AttentionPendingError
	if errors.As(err, &pending) {
		return "", &taskengine.ApprovalPendingError{ApprovalID: pending.AskID, ToolName: missiontools.ToolNameAskAttention}
	}
	return answer, err
}

// publishAsked emits the attention-asked event, best-effort: a publish
// failure must never fail the ask, since it's still answerable from the queue.
func (a missionAttentionAsker) publishAsked(ctx context.Context, ev missionservice.AttentionAskedEvent) {
	if a.bus == nil {
		return
	}
	raw, err := json.Marshal(ev)
	if err != nil {
		return
	}
	_ = a.bus.Publish(ctx, missionservice.AttentionAskedSubject, raw)
}

func acpPolicySource() hitlservice.PolicySource {
	home, err := os.UserHomeDir()
	if err != nil {
		return hitlservice.NewFSPolicySource()
	}
	return hitlservice.NewFSPolicySource(filepath.Join(home, ".contenox"))
}

// acpUpdateBanner returns a one-line update banner, or "" if none is
// available or the operator opted out. Non-blocking: waits at most 500ms,
// so a slow network call is silently skipped and retried next session.
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

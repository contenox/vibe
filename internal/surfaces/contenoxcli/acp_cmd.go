package contenoxcli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/contenox/contenox/internal/kernel/enginesvc"
	"github.com/contenox/contenox/internal/kernel/reasoning"
	"github.com/contenox/contenox/internal/models/modelrepo"
	"github.com/contenox/contenox/internal/services/agentregistryservice"
	"github.com/contenox/contenox/internal/services/agentservice"
	"github.com/contenox/contenox/internal/services/eventlog"
	"github.com/contenox/contenox/internal/services/fleetservice"
	"github.com/contenox/contenox/internal/services/hitlservice"
	"github.com/contenox/contenox/internal/services/localtools"
	"github.com/contenox/contenox/internal/services/missionservice"
	"github.com/contenox/contenox/internal/services/oracletools"
	"github.com/contenox/contenox/internal/services/presence"
	"github.com/contenox/contenox/internal/services/updatecheck"
	"github.com/contenox/contenox/internal/services/vfs"
	"github.com/contenox/contenox/internal/store/runtimetypes"
	"github.com/contenox/contenox/internal/substrate"
	"github.com/contenox/contenox/internal/surfaces/acpsvc"
	"github.com/contenox/contenox/internal/surfaces/fleetboot"
	"github.com/contenox/contenox/libacp"
	libdb "github.com/contenox/contenox/libdbexec"
	"github.com/contenox/contenox/liblog"
	"github.com/contenox/contenox/libtracker"
	"github.com/spf13/cobra"
)

var acpCmd = &cobra.Command{
	Use:   "acp",
	Short: "Run the Contenox ACP server over stdio.",
	Long: `Speak Agent Client Protocol over stdio so editors like Zed can run local Contenox chains.

The editor owns the working directory: per the protocol, the cwd it sends on
session/new is that session's workspace, so this command configures no workspace
root of its own. Filesystem and terminal tools are proxied back to the editor
over the client capabilities it declared, which is why a client that grants
neither is served neither.

The chain executed for each session/prompt is compiled from its agents/ declaration into
~/.contenox/.generated/chain-agent-acp.json (an operator copy at ~/.contenox/chain-agent-acp.json
wins if present, then .generated/, then the system/ fallback). Override the path entirely with
the CONTENOX_ACP_CHAIN_PATH environment variable.

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
device owner — OpenClaw and other non-editor clients. It runs under the hardened
'acpx' envelope from agents.toml, rendered to hitl-policy-acpx.json (writes and
the shell denied, secret paths refused on read, anything unnamed denied — pass
--hitl-policy to name another), and the chain compiled from its agents/ declaration into
~/.contenox/.generated/chain-agent-acpx.json (operator copy, then .generated/,
then system/ — override the path entirely with CONTENOX_ACPX_CHAIN_PATH).

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
		registerHITLPolicyFlag(c)
	}
	acpCmd.Flags().Bool("experimental-acp", false, "Accepted for compatibility with ACP clients that hardcode this launch flag (e.g. AionUi); no effect.")
	_ = acpCmd.Flags().MarkHidden("experimental-acp")
}

type acpStdio struct{}

func (acpStdio) Read(p []byte) (int, error)  { return os.Stdin.Read(p) }
func (acpStdio) Write(p []byte) (int, error) { return os.Stdout.Write(p) }
func (acpStdio) Close() error                { return os.Stdin.Close() }

type acpProfile struct {
	// hitlEnvelope is the [envelopes.<name>] section this surface runs under,
	// which is also the stem of the policy file it resolves.
	hitlEnvelope string
	chainFile    string
	chainEnv     string
	embedFleet   bool
	seedFIMChain func(contenoxDir string) error
	// host runs a long-lived host instead of an ACP connection over stdio.
	host bool
	beam bool
	name string
}

var acpProfileACP = acpProfile{
	hitlEnvelope: "default",
	chainFile:    chainAgentACPFilename,
	chainEnv:     "CONTENOX_ACP_CHAIN_PATH",
	embedFleet:   true,
	seedFIMChain: seedFIMChainIfMissing,
	name:         "acp",
}

// acpProfileServe is the editor profile with the stdio connection removed.
var acpProfileServe = acpProfile{
	hitlEnvelope: "serve",
	chainFile:    chainAgentACPFilename,
	chainEnv:     "CONTENOX_ACP_CHAIN_PATH",
	embedFleet:   true,
	seedFIMChain: seedFIMChainIfMissing,
	host:         true,
	name:         "serve",
}

var acpProfileBeam = acpProfile{
	hitlEnvelope: "default",
	chainFile:    chainAgentACPFilename,
	chainEnv:     "CONTENOX_ACP_CHAIN_PATH",
	embedFleet:   true,
	beam:         true,
	name:         "beam",
}

var acpProfileACPX = acpProfile{
	hitlEnvelope: "acpx",
	chainFile:    chainAgentACPXFilename,
	chainEnv:     "CONTENOX_ACPX_CHAIN_PATH",
	name:         "acpx",
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

	logDest := io.Writer(os.Stderr)
	var surfaceLog *liblog.Writer
	if profile.host || profile.beam {
		rot, logErr := openHostLog(cmd, profile.name)
		switch {
		case logErr == nil:
			surfaceLog = rot
			logDest = rot
			defer rot.Close()
		case profile.beam:
			fmt.Fprintf(os.Stderr, "contenox beam: logging is unavailable, running without logs: %v\n", logErr)
			logDest = io.Discard
		default:
			fmt.Fprintf(os.Stderr, "contenox %s: logging to a file is unavailable, using stderr: %v\n", profile.name, logErr)
		}
	}
	noticeOut := io.Writer(os.Stderr)
	if profile.beam {
		noticeOut = logDest
	}
	var tracker libtracker.ActivityTracker = libtracker.NewTextActivityTracker(logDest)

	reportErr, reportChange, endStartup := tracker.Start(ctx, "startup", profile.name)
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

	// The host log booted on defaults before the database was readable.
	if surfaceLog != nil {
		applyStoredLogSettings(ctx, runtimetypes.New(db.WithoutTransaction()), surfaceLog)
		reportChange("phase", "apply_log_settings")
	}

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
	// Every declared envelope, not just this profile's: the mission envelopes
	// this surface offers and the names /policy accepts are resolved by
	// filename, so they have to be rendered before a session asks for one. The
	// profile's own is ensured again below, where a failure is fatal.
	if _, err := syncEnvelopePolicies(contenoxDir); err != nil {
		fmt.Fprintf(noticeOut, "contenox acp: rendering envelopes: %v\n", err)
	}
	reportChange("phase", "render_envelopes")
	seedOptionalFIMChain(profile, contenoxDir)
	reportChange("phase", "seed_fim_chain")

	closeLogs, err := setupTelemetryLogging(ctx, runtimetypes.New(db.WithoutTransaction()), contenoxDir, logDest)
	if err != nil {
		reportErr(err)
		return fmt.Errorf("setup telemetry logging: %w", err)
	}
	defer closeLogs()
	reportChange("phase", "setup_telemetry")

	// transport is this process's own stdio connection; a host has none, and
	// every session it serves arrives over the relay instead.
	var transport *acpsvc.Transport

	// One registry for every connection, so an approval raised by remote work is
	// answered on the connection that drives the session.
	sessionRouter := acpsvc.NewSessionRouter()

	if acpsvc.ReadConfigValue(ctx, db, "default-model") == "" &&
		(os.Getenv(envDefaultProvider) != "" || os.Getenv(envDefaultModel) != "") {
		if err := completeEnvSetup(ctx, db); err != nil {
			fmt.Fprintf(noticeOut, "contenox acp: environment-based setup incomplete: %v\n", err)
		} else {
			fmt.Fprintln(noticeOut, "contenox acp: configured provider/model from environment.")
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

	if err := ensureProfileChain(ctx, contenoxDir, profile.chainFile, profile.chainEnv, tracker); err != nil {
		reportErr(err)
		return err
	}
	reportChange("phase", "ensure_profile_chain")

	policy, err := resolveProfilePolicy(ctx, cmd, contenoxDir, profile.hitlEnvelope, tracker)
	if err != nil {
		reportErr(err)
		return err
	}
	reportChange("phase", "ensure_profile_policy")

	chains, err := acpsvc.LoadChainRegistryFrom(profile.chainFile, profile.chainEnv)
	if err != nil {
		return err
	}
	_, _, end := tracker.Start(ctx, "load", "acp_chain", "source", chains.Source(), "id", chains.Default().ID)
	end()
	fimChains := loadOptionalFIMChain(ctx, tracker, profile)

	// The engine doesn't own the bus, so this defer closes it.
	bus, err := substrate.OpenBus(ctx, db.WithoutTransaction())
	if err != nil {
		reportErr(err)
		return err
	}
	defer bus.Close()
	kvMgr, releaseKV, err := substrate.OpenKV(ctx, db)
	if err != nil {
		reportErr(err)
		return err
	}
	defer releaseKV()
	trigHook := eventlog.NewTriggerHolder()
	missionPub := missionEventPublisher(ctx, db, bus, workspaceID, tracker, trigHook)
	missions := missionservice.New(db, missionservice.WithEventPublisher(missionPub))

	acpHITL := hitlservice.NewWithDefaultPolicy(policy.source(contenoxDir), runtimetypes.LocalTenantID, runtimetypes.New(db.WithoutTransaction()), tracker, policy.Name)
	// /policy and `contenox config set hitl-policy-name` both write this workspace's row; the evaluator must read the same one.
	hitlservice.SetWorkspaceID(acpHITL, workspaceID)
	askBridge := newRelayAskBridge(acpHITL, tracker)
	hitlservice.SetAskWatcher(acpHITL, askBridge)
	resumeBridge := newRelayResumeBridge(tracker)

	// Assigned once the fleet is built below; the toolset resolves it per call.
	var inProcessFleet fleetservice.Service

	tools := acpToolset(profile, db, tracker, workspaceID,
		routedTransport(sessionRouter, func() *acpsvc.Transport { return transport }),
		missions, acpHITL, missionPub, optInBeta,
		func() fleetservice.Service { return inProcessFleet })
	printUnservedToolsets(noticeOut, unservedToolsets(chains.Default(), tools))

	oracleStore := runtimetypes.New(db.WithoutTransaction())
	oracleCfg := resolveOracleConfig(ctx, oracleStore, cmd)
	// A dispatched unit does not adjudicate: the host that fired it holds the
	// mission envelope and rules on its gated calls.
	if strings.TrimSpace(os.Getenv(profile.chainEnv)) != "" {
		oracleCfg.chain = ""
	}
	if oracleCfg.enabled() {
		tools[oracletools.ToolsProviderName] = oracletools.New(oracleResolver{
			hitl: acpHITL, missions: missions, store: oracleStore, out: noticeOut,
		})
	}

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
		fmt.Fprintln(noticeOut, "contenox acp: no default-model configured; serving setup-only. Run the \"Setup Contenox\" auth method or `contenox acp --setup` to configure a provider and model.")
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
			KVStore:            kvMgr,
		}
		if enableHITL {
			cfg.EnableHITL = true
			cfg.AskApproval = askApproval
			cfg.HITLPolicySource = policy.source(contenoxDir)
			cfg.HITLDefaultPolicyName = policy.Name
			// The process's one durable-ask service: two instances cannot wake each other's parked waiters.
			cfg.HITLService = acpHITL
		}

		engine, err = enginesvc.Build(ctx, db, cfg)
		if err != nil {
			return fmt.Errorf("build engine: %w", err)
		}
		defer engine.Stop()
		// A verdict landing with no waiter parked resumes the suspended run here,
		// and a triggered run's outcome goes back to the relay from there.
		hitlservice.SetResumeHook(acpHITL, resumeBridge.hook(agentservice.Deps{
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
				out:           noticeOut,
			})
			fmt.Fprintln(noticeOut, oracleMountedLine(oracleCfg))
		}
		trigHook.Set(buildInProcessTriggerHook(ctx, db, contenoxDir, workspaceID, engine, triggerOpts, noticeOut))
		// Deferred AFTER engine.Stop so LIFO drains in-flight firings before teardown.
		defer trigHook.Drain(eventlog.DefaultDrainTimeout)
	}

	updateBanner := acpUpdateBanner(dbCtx, db, contenoxDir)

	presenceStore := presence.NewStore(kvMgr)

	// A dispatched unit must not host its own fleet: it would double-route its
	// report and recursively spawn fleets.
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
			// Late-bound: nil until the conn factory runs below.
			Transport:    func() *acpsvc.Transport { return transport },
			HITL:         acpHITL,
			PolicySource: policy.source(contenoxDir),
			DiscoverAgents: func(dctx context.Context, agents agentregistryservice.Service) {
				discoverChainAgents(dctx, agents, contenoxDir, tracker, DiscoverDeps{Store: runtimetypes.New(db.WithoutTransaction()), Bus: bus})
			},
			WorkspaceID: workspaceID,
			DBPath:      dbPath,
		})
		if buildErr != nil {
			return buildErr
		}
		missionFleet, missionAgents, stopFleetTeardown = fleet, agents, stop
		inProcessFleet = fleet
	}
	if stopFleetTeardown != nil {
		// Children die with the parent.
		defer stopFleetTeardown()
	}

	launchDir, err := os.Getwd()
	if err != nil {
		reportErr(err)
		return fmt.Errorf("resolve working directory: %w", err)
	}
	// serve and beam are both single-workspace HOSTS: each serves exactly one
	// workspace, fixed here for its lifetime, so a session created against it —
	// by an app, an event trigger, or its own operator — resolves the "/"
	// sentinel to that one root. Only the editor profile (acp) builds no
	// factory: there the editor owns the cwd and sends a real path per session,
	// per the protocol.
	defaultRoot := launchDir
	var workspaceRoots *vfs.Factory
	switch {
	case profile.host:
		defaultRoot = defaultHostRoot(cmd, launchDir)
		workspaceRoots, err = buildWorkspaceFactory(defaultRoot)
		if err != nil {
			reportErr(err)
			return err
		}
	case profile.beam:
		defaultRoot, err = beamRoot(cmd, launchDir)
		if err != nil {
			reportErr(err)
			return err
		}
		workspaceRoots, err = buildWorkspaceFactory(defaultRoot)
		if err != nil {
			reportErr(err)
			return err
		}
	}

	transportFactory := acpsvc.New(acpsvc.Deps{
		Engine:                engine,
		DB:                    db,
		WorkspaceRoots:        workspaceRoots,
		ChainRegistry:         chains,
		FIMChainRegistry:      fimChains,
		DefaultModel:          defaultModel,
		DefaultProvider:       defaultProvider,
		DefaultAltModel:       defaultAltModel,
		DefaultAltProvider:    defaultAltProvider,
		DefaultMaxTokens:      defaultMaxTokens,
		DefaultThink:          defaultThink,
		WorkspaceID:           workspaceID,
		ContenoxDir:           contenoxDir,
		SessionRouter:         sessionRouter,
		KnownPolicies:         knownPolicyNames(contenoxDir),
		HITLDefaultPolicyName: policy.Name,
		UpdateBanner:          updateBanner,
		Fleet:                 missionFleet,
		Agents:                missionAgents,
		Asks:                  acpHITL,
		MissionEnvelopes:      newMissionEnvelopes(contenoxDir),
		OptInBeta:             optInBeta,
		EnvSetup: &acpsvc.EnvSetupSpec{
			Vars: acpEnvSetupVars(),
			Complete: func(cctx context.Context) error {
				return completeEnvSetup(cctx, db)
			},
		},
	})

	stopRelay := serveRemoteAttachments(ctx, contenoxDir, transportFactory,
		buildRelayChainTriggers(db, contenoxDir, workspaceID, engine, triggerOpts, resumeBridge), askBridge, tracker, noticeOut)
	defer stopRelay()

	acpCwd, _ := os.Getwd()
	presenceKind := presence.KindACP
	if profile.host {
		presenceKind = presence.KindServe
	}
	presenceReporter := presence.StartReporter(ctx, presenceStore, presence.Record{
		Kind: presenceKind,
		Cwd:  acpCwd,
	})
	defer presenceReporter.Stop()

	if profile.beam {
		return runBeamSurface(ctx, cmd, beamSurface{
			factory:       transportFactory,
			bindTransport: func(t *acpsvc.Transport) { transport = t },
			reporter:      presenceReporter,
			bus:           bus,
			root:          defaultRoot,
			model:         defaultModel,
			provider:      defaultProvider,
			engineReady:   engine != nil,
			logPath:       surfaceLogPath(surfaceLog),
		})
	}

	// A host has no stdin to serve ACP over; the relay tunnel is its only
	// inbound path.
	if profile.host {
		return runHost(ctx, cmd, hostScreen{
			contenoxDir:  contenoxDir,
			workspaceID:  workspaceID,
			root:         defaultRoot,
			model:        defaultModel,
			provider:     defaultProvider,
			engineReady:  engine != nil,
			setupCheck:   hostSetupCheck(ctx, engine),
			log:          surfaceLog,
			relayEnabled: relayIsConfigured(contenoxDir),
		})
	}

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

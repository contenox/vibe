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

	"github.com/contenox/beam/internal/kernel/agentinstance"
	"github.com/contenox/beam/internal/kernel/enginesvc"
	"github.com/contenox/beam/internal/kernel/reasoning"
	"github.com/contenox/beam/internal/kernel/taskengine"
	"github.com/contenox/beam/internal/libbus"
	libdb "github.com/contenox/beam/internal/libdbexec"
	"github.com/contenox/beam/internal/libkvstore"
	"github.com/contenox/beam/internal/libtracker"
	"github.com/contenox/beam/internal/models/modelrepo"
	"github.com/contenox/beam/internal/services/agentregistryservice"
	"github.com/contenox/beam/internal/services/fleetservice"
	"github.com/contenox/beam/internal/services/hitlservice"
	"github.com/contenox/beam/internal/services/localtools"
	"github.com/contenox/beam/internal/services/missionservice"
	"github.com/contenox/beam/internal/services/missiontools"
	"github.com/contenox/beam/internal/services/presence"
	"github.com/contenox/beam/internal/services/reportrouter"
	"github.com/contenox/beam/internal/services/updatecheck"
	"github.com/contenox/beam/internal/store/runtimetypes"
	"github.com/contenox/beam/internal/surfaces/acpsvc"
	"github.com/contenox/beam/libacp"
	"github.com/spf13/cobra"
)

var acpCmd = &cobra.Command{
	Use:   "acp",
	Short: "Run the Contenox ACP server over stdio.",
	Long: `Speak Agent Client Protocol over stdio so editors like Zed can run local Contenox chains.

The chain executed for each session/prompt is loaded from ~/.contenox/default-acp-chain.json
(override with the CONTENOX_ACP_CHAIN_PATH environment variable). Populate it like any other
contenox chain.

The default model is read from the global 'default-model' / 'default-provider' configuration
(set via 'contenox config set default-model …'). Logging goes to stderr; stdin/stdout are
reserved for the JSON-RPC stream.

HITL is on by default — gated tool calls route through the ACP session/request_permission
flow so the editor's UI controls approval. Pass --auto to disable (unattended/testing).

The /mission slash command dispatches a mission in-process: the fired unit is a child
subprocess of this editor session and its reports arrive live back in the firing session.
No 'contenox serve' is needed. Setting CONTENOX_SERVER_URL opts into forwarding the
dispatch to that serve instead (reports then land in its operator inbox).`,
	RunE: runACP,
}

var acpxCmd = &cobra.Command{
	Use:   "acpx",
	Short: "Run as an ACP agent under the headless / untrusted-driver profile (OpenClaw).",
	Long: `Same Agent Client Protocol server as 'acp', for drivers that are not the
device owner — OpenClaw and other non-editor clients. It loads the hardened
hitl-policy-acpx.json (local_shell denied, web mutations denied, web reads
gated) and the chain at ~/.contenox/headless-acp-chain.json (override with
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
	// embedFleet gives this profile the `/mission` slash command. The editor
	// profile (acp — the Zed journey) embeds the fleet IN-PROCESS so a mission is a
	// subagent of THIS editor, reporting back live into the firing session (see the
	// mission block in runACPProfile). DISABLED for acpx: that profile is the hardened
	// surface for an untrusted driver (OpenClaw), which must not be handed a lever
	// to dispatch fleet units at all — neither in-process nor forwarded.
	embedFleet bool
}

var acpProfileACP = acpProfile{
	hitlPolicy: "hitl-policy-acp.json",
	chainFile:  "default-acp-chain.json",
	chainEnv:   "CONTENOX_ACP_CHAIN_PATH",
	seedChain:  seedACPChainIfMissing,
	embedFleet: true,
}

var acpProfileACPX = acpProfile{
	hitlPolicy: "hitl-policy-acpx.json",
	chainFile:  headlessACPChainFilename,
	chainEnv:   "CONTENOX_ACPX_CHAIN_PATH",
	seedChain:  seedHeadlessACPChainIfMissing,
}

func runACP(cmd *cobra.Command, _ []string) error  { return runACPProfile(cmd, acpProfileACP) }
func runACPX(cmd *cobra.Command, _ []string) error { return runACPProfile(cmd, acpProfileACPX) }

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

	// Deferred before the engine is built so it runs after engine teardown
	// (LIFO): drain registered model-backend shutdown hooks (e.g. native
	// in-process inference sessions) deterministically on exit. No-op when no
	// hook is registered.
	defer func() { _ = modelrepo.Shutdown() }()

	autoMode, _ := cmd.Flags().GetBool("auto")
	enableHITL := !autoMode

	workspaceFlag, _ := cmd.Flags().GetString("workspace-id")

	// Create tracker early for full startup telemetry (using text to stderr for ACP/CLI).
	// This instruments the entire launch so we can see phase timings and errors on freezes.
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

	// Anchor seeding/telemetry to the SAME directory the ACP runtime reads from.
	// The DB (globalDBPath), the chain (LoadChainRegistryFrom) and HITL policies
	// (acpPolicySource) all resolve to $HOME/.contenox via os.UserHomeDir(),
	// ignoring ResolveContenoxDir's cwd-walk. Using that cwd-walk here meant a
	// launch from an arbitrary working directory (Zed's project dir, or the ACP
	// registry's isolated sandbox) seeded presets into <cwd>/.contenox while the
	// loaders looked in $HOME/.contenox — so the chain/policy presets were
	// silently absent and `acp` hard-errored before serving initialize.
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

	closeLogs, err := setupTelemetryLogging(ctx, runtimetypes.New(db.WithoutTransaction()), contenoxDir)
	if err != nil {
		reportErr(err)
		return fmt.Errorf("setup telemetry logging: %w", err)
	}
	defer closeLogs()
	reportChange("phase", "setup_telemetry")

	var transport *acpsvc.Transport

	// Environment-based setup: when nothing is configured yet but the launch
	// environment names a provider/model (an editor config or the ACP env_var
	// auth flow relaunching us), persist that configuration exactly as the
	// interactive wizard would. A failure is not fatal — we fall through to
	// setup-only mode, whose auth methods explain what is missing.
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

	// The ONE bus this process owns, over the shared $HOME/.contenox/local.db. It is
	// created here — before the tools and the engine — because the mission tools
	// need a publisher at construction time, and the fleet's report router (embedded
	// below) needs the same subject stream. Sharing it is deliberate: the engine
	// would otherwise mint its OWN bus (enginesvc.Build, when Config.Bus is nil), and
	// the mission tools a THIRD, so a single shared bus is what "don't double up"
	// means here. Closed on shutdown; the engine does NOT own it (ownsBus stays
	// false when Config.Bus is supplied), so engine.Stop leaves it for this defer.
	bus := libbus.NewSQLite(db.WithoutTransaction())
	defer bus.Close()
	// The durable mission store, publisher-wired so AddReport emits a
	// ReportAddedEvent. In the DISPATCHER (editor) process the embedded report
	// router below consumes it; in a dispatched UNIT process the same publisher is
	// what carries the unit's own report across the process boundary to whichever
	// process fired it. Shared between the mission tools here and the fleetservice
	// embedded below, exactly as serve shares one missionservice.
	missions := missionservice.New(db, missionservice.WithEventPublisher(bus))

	// One durable-ask service for this process: the unit half raises questions
	// through it, the supervisor half answers them through it.
	acpHITL := hitlservice.NewWithDefaultPolicy(acpPolicySource(), runtimetypes.LocalTenantID, runtimetypes.New(db.WithoutTransaction()), tracker, profile.hitlPolicy)

	tools := map[string]taskengine.ToolsRepo{
		"echo":     localtools.NewEchoTools(),
		"print":    localtools.NewPrint(tracker),
		"webtools": localtools.NewWebCaller(tracker),
		"local_fs": localtools.NewLocalFSToolsWith(
			"",
			db,
			acpsvc.NewACPFileIO(func() *acpsvc.Transport { return transport }),
			"local_fs",
			acpsvc.NewACPCwdResolver(func() *acpsvc.Transport { return transport }),
		),
		"local_shell": localtools.NewLocalExecToolsWith(
			acpsvc.NewACPCommandRunner(func() *acpsvc.Transport { return transport }),
		),
		// Mission tools: the per-mission report/ask-for-attention channel a
		// dispatched unit holds while running unattended. Registered here because
		// a fleet unit IS a `contenox acp` subprocess, and the mission tools are
		// its OWN local providers writing to the shared mission store (this db,
		// under the same $HOME/.contenox the dispatcher reads). The grant is still
		// per-mission: the provider exposes nothing and executes nothing unless the
		// session was constructed with a mission id (session/new `_meta`), so an
		// ordinary editor session over `contenox acp` never sees them. The nil
		// asker means mission_ask_attention records a durable blocker report rather
		// than a durable permission ask — wiring the ask to the operator inbox from
		// a viewer-less unit is fleet-consolidation.md's M5, a separate slice.
		//
		// It shares the publisher-wired `missions` created above: report routing
		// runs in the DISPATCHER's process (the report router this editor embeds
		// below, OR serve's), subscribed to the SQLite bus over this same shared
		// $HOME/.contenox/local.db, and it routes purely off ReportAddedEvent. A
		// publisher-less service here would store a unit's report durably but never
		// publish, so the supervision edge would silently go nowhere — the exact
		// cross-process seam the composed round-trip e2e keeps closed.
		// The asker is what turns mission_ask_attention from a one-way flare into a
		// question: it raises the unit's question as a durable ask an operator can
		// see and ANSWER, and hands the answer back as the tool's result. Wired over
		// the same store the approvals API and `contenox approvals` already read —
		// no second mechanism — and cross-process by construction: the unit raises
		// the ask here, the operator answers it in the serve process, and both meet
		// on the one shared database (see hitlservice.RequestAttention).
		missiontools.ToolsProviderName: missiontools.New(missions, missionAttentionAsker{
			hitl:     acpHITL,
			missions: missions,
			bus:      bus,
		}, missiontools.WithSupervision(
			// The other direction: a session in THIS process that fires missions can
			// see its units and answer what they ask. Both surfaces come from one
			// provider because they are one relationship seen from its two ends.
			missionSupervision{missions: missions, hitl: acpHITL},
			missionSupervision{missions: missions, hitl: acpHITL},
		)),
	}

	var askApproval localtools.AskApproval
	if enableHITL {
		askApproval = func(ctx context.Context, req hitlservice.ApprovalRequest) (bool, error) {
			if transport == nil {
				return false, fmt.Errorf("acpsvc: HITL approval requested before transport initialization")
			}
			return transport.AskApproval(ctx, req)
		}
	}

	// The engine requires a configured model: enginesvc.Build wires the embed/
	// task/chat executors and EnsureModels, all of which reject an empty model
	// name. When none is set yet we serve a setup-only transport instead of
	// hard-exiting: initialize/authenticate still work, so an ACP client can run
	// the "Setup Contenox" terminal auth method (`acp --setup`) to configure one.
	// Session creation returns an actionable error until then (see acpsvc).
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
			// Reuse the one bus (see above), so the engine does not mint a second.
			Bus: bus,
		}
		if enableHITL {
			cfg.EnableHITL = true
			cfg.AskApproval = askApproval
			cfg.HITLPolicySource = acpPolicySource()
			cfg.HITLDefaultPolicyName = profile.hitlPolicy
		}

		engine, err = enginesvc.Build(ctx, db, cfg)
		if err != nil {
			return fmt.Errorf("build engine: %w", err)
		}
		defer engine.Stop()
	}

	updateBanner := acpUpdateBanner(dbCtx, db, contenoxDir)

	// Fleet presence store (shared-SQLite over the same $HOME/.contenox/local.db
	// serve reads): serve REGISTERS its reachable address here; this process
	// self-registers below. Presence is now board-only telemetry — the `/mission`
	// forwarder no longer discovers a serve through it (forwarding is the explicit
	// CONTENOX_SERVER_URL opt-in), so a serve's Address publishes here harmlessly.
	presenceStore := presence.NewStore(libkvstore.NewSQLiteManager(db))

	// The `/mission` slash command. A mission is a SUBAGENT of the process that
	// fired it: the
	// editor embeds the fleet IN-PROCESS and dispatches there BY DEFAULT — the fired
	// unit is a child subprocess of THIS process, and its report comes back LIVE
	// into the firing session (missionReportDeliverer, via the report router). Mission
	// lifetime ≤ this process's lifetime: kernel teardown on shutdown reaps the child.
	// Forwarding to a serve survives ONLY as an explicit opt-in (CONTENOX_SERVER_URL
	// set) for firing onto a bigger box.
	//
	// Two guards shape which path — if any — is wired:
	//   - embedFleet gates the whole thing to the editor profile (acpx, the
	//     untrusted driver, gets no mission lever at all).
	//   - a DISPATCHED UNIT must not host its own fleet. It would double-route its
	//     own report — delivered live by its parent AND inboxed here as parent-gone,
	//     since the SQLite bus BROADCASTS every event to every subscriber — and would
	//     recursively spawn fleets. A unit IS a `contenox acp` bound to a specific
	//     chain via the chain env (chainSpawner sets it for chain agents; declared
	//     mission agents set it too), so that env being set is exactly the signal
	//     "this process is itself a unit" — it hosts no fleet of its own.
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
		// IN-PROCESS fleet (the default editor journey — needs a configured model,
		// since the dispatched unit resolves the same $HOME state this editor runs on).
		fleet, agents, stop, buildErr := buildInProcessFleet(ctx, inProcessFleetDeps{
			db:          db,
			bus:         bus,
			missions:    missions,
			contenoxDir: contenoxDir,
			tracker:     tracker,
			transport:   func() *acpsvc.Transport { return transport },
			hitl:        acpHITL,
		})
		if buildErr != nil {
			return buildErr
		}
		missionFleet, missionAgents, stopFleetTeardown = fleet, agents, stop
	}
	if stopFleetTeardown != nil {
		// Children die with the parent: stop the report router and Close the kernel
		// (killing every dispatched child subprocess) on process shutdown — the
		// ontology's "mission lifetime ≤ acp process lifetime".
		defer stopFleetTeardown()
	}

	transportFactory := acpsvc.New(acpsvc.Deps{
		Engine:                engine,
		DB:                    db,
		ChainRegistry:         chains,
		DefaultModel:          defaultModel,
		DefaultProvider:       defaultProvider,
		DefaultAltModel:       defaultAltModel,
		DefaultAltProvider:    defaultAltProvider,
		DefaultMaxTokens:      defaultMaxTokens,
		DefaultThink:          defaultThink,
		WorkspaceID:           workspaceID,
		ContenoxDir:           contenoxDir,
		KnownPolicies:         embeddedPolicyNames(),
		HITLDefaultPolicyName: profile.hitlPolicy,
		UpdateBanner:          updateBanner,
		// `/mission` dispatch: the in-process fleet, or nil (acpx, a dispatched
		// unit, or a setup-only editor) — nil-safe throughout acpsvc when unwired.
		Fleet:  missionFleet,
		Agents: missionAgents,
		EnvSetup: &acpsvc.EnvSetupSpec{
			Vars: acpEnvSetupVars(),
			Complete: func(cctx context.Context) error {
				return completeEnvSetup(cctx, db)
			},
		},
	})

	// Fleet presence: make THIS editor-spawned process visible on the fleet board.
	// It self-registers into the shared-SQLite presence store (the same $HOME/
	// .contenox/local.db serve reads), heartbeating on a modest interval and on
	// session events. Entirely best-effort — a presence write never blocks or
	// fails serving the editor (see runtime/presence). The decorator around the
	// transport feeds it the client name (from initialize) and the open-session
	// count without acpsvc needing to know presence exists.
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

// inProcessFleetDeps are the collaborators buildInProcessFleet wires the
// editor's embedded fleet from — all over the SAME shared db handle and bus the
// acp process already opened.
type inProcessFleetDeps struct {
	db          libdb.DBManager
	bus         libbus.Messenger
	missions    missionservice.Service
	contenoxDir string
	tracker     libtracker.ActivityTracker
	// transport late-binds this connection's live acpsvc.Transport (nil until the
	// conn factory runs), so the report deliverer can reach the firing editor
	// session a mission was fired from.
	transport func() *acpsvc.Transport
	// hitl is the durable ask store this process raises and answers questions
	// through — the same instance the mission tools use, so the supervisor's
	// answer and the unit's question meet in one place.
	hitl hitlservice.Service
}

// buildInProcessFleet embeds the fleet the standalone editor dispatches
// `/mission` through — the ontology's in-process subagent kernel (a mission is
// a subagent of THIS process). The composition itself lives in the service
// layer (fleetservice.BuildInProcess — the build-on-services rule); this
// adapter contributes only what the EDITOR surface knows:
//
//   - live-parent delivery through this connection's late-bound transport
//     (missionReportDeliverer: the live editor session first, the kernel second);
//   - the autonomous answer edge serve has too — a unit's question may be
//     offered to the agent driving the session that fired it, when that
//     mission's envelope allows. Without this an editor-fired mission could
//     ask, and be answered by a human, but never by the very agent holding the
//     conversation the mission came from — the case this is most useful in,
//     since Zed and other ACP clients render no answer box of their own;
//   - chain-agent discovery over this editor's .contenox dirs, and the same
//     envelope-existence guard the serve path enforces over its policy files,
//     so `/mission --policy typo.json` is refused here too.
func buildInProcessFleet(ctx context.Context, deps inProcessFleetDeps) (fleetservice.Service, agentregistryservice.Service, func(), error) {
	// A dispatched mission's cwd defaults to this editor's working directory (the
	// project Zed launched us in) when the request names none.
	projectRoot, _ := os.Getwd()
	return fleetservice.BuildInProcess(ctx, fleetservice.InProcessDeps{
		DB:           deps.db,
		Bus:          deps.bus,
		Missions:     deps.missions,
		ProjectRoot:  projectRoot,
		Tracker:      deps.tracker,
		PolicySource: hitlPolicySource(deps.contenoxDir),
		DiscoverAgents: func(dctx context.Context, agents agentregistryservice.Service) {
			discoverChainAgents(dctx, agents, deps.contenoxDir)
		},
		SessionDeliverer: func(kernel agentinstance.Manager) reportrouter.SessionDeliverer {
			return missionReportDeliverer{
				chat:   func() contenoxSessionDeliverer { return chatDeliverer(deps.transport()) },
				kernel: kernel,
			}
		},
		AgentSupervisor: agentAnswerOffer{
			hitl:     deps.hitl,
			missions: deps.missions,
			prompter: transportPrompter{transport: deps.transport},
			tracker:  deps.tracker,
		},
		Stderr: os.Stderr,
	})
}

// missionReportDeliverer is the report router's SessionDeliverer for the
// IN-PROCESS editor topology. Per the governing ontology, a mission is a
// subagent of the editor process that fired it, and its
// report must reach THAT parent — which, for a `/mission` fired from the editor,
// is one of THIS process's own native stdio sessions, NOT a kernel-owned unit. So
// the live editor transport is tried FIRST (Transport.DeliverToContenoxSession
// maps the firing session's contenox id onto the ACP connection and pushes the
// update); the kernel is tried second, for the rarer case of a mission fired by
// an in-process kernel unit's own session. When neither owns the firing session —
// it has ended, or was never here — both miss and the report router inboxes the
// report (the true no-live-parent fallback). This is exactly the live delivery
// the serve-forwarded topology could not make: there the firing session lived in
// a different process, so the report fell to the inbox as parent-gone.
type missionReportDeliverer struct {
	// chat resolves the ACP surface that may own the firing session, late-bound
	// because the editor's lone transport does not exist yet when the router is
	// built. It returns nil when there is none to ask. Both shapes of that surface
	// implement the same one method: the editor's single *acpsvc.Transport, and
	// serve's *acpsvc.SessionRouter, which finds the right one of its many
	// connections by the same contenox session id.
	chat   func() contenoxSessionDeliverer
	kernel agentinstance.Manager
}

// contenoxSessionDeliverer injects an out-of-band update into a chat session
// addressed by its CONTENOX (internal) session id — the id a mission carries as
// its ParentSessionID. Declared here, where both implementations are consumed.
type contenoxSessionDeliverer interface {
	DeliverToContenoxSession(ctx context.Context, contenoxSessionID string, n libacp.SessionNotification) error
}

// chatDeliverer adapts a possibly-nil *acpsvc.Transport to the interface. The
// explicit nil check is load-bearing: returning a nil *Transport as an interface
// value yields a NON-nil interface holding a nil pointer, which would sail past
// the caller's nil guard and panic on the first delivery.
func chatDeliverer(t *acpsvc.Transport) contenoxSessionDeliverer {
	if t == nil {
		return nil
	}
	return t
}

var _ reportrouter.SessionDeliverer = missionReportDeliverer{}

func (d missionReportDeliverer) DeliverToSession(ctx context.Context, sessionID libacp.SessionID, n libacp.SessionNotification) error {
	if d.chat != nil {
		if chat := d.chat(); chat != nil {
			if err := chat.DeliverToContenoxSession(ctx, string(sessionID), n); err == nil {
				return nil
			}
		}
	}
	if d.kernel != nil {
		return d.kernel.DeliverToSession(ctx, sessionID, n)
	}
	return acpsvc.ErrSessionNotLive
}

// missionAttentionAsker adapts the durable-ask machinery to
// missiontools.AttentionAsker: a unit's question becomes a pending ask, the wait
// blocks until an operator answers it (or the ceiling expires it), and the
// operator's own words are returned to the asking unit as its tool result.
//
// It lives at the composition root, like missionReportDeliverer above, because
// it is the one place that holds both halves — the tool package deliberately
// depends on nothing but its one-method seam, and hitlservice knows nothing
// about missions' tools.
type missionAttentionAsker struct {
	hitl     hitlservice.Service
	missions missionservice.Service
	bus      libbus.Messenger
}

var _ missiontools.AttentionAsker = missionAttentionAsker{}

func (a missionAttentionAsker) RaiseAttention(ctx context.Context, missionID, summary, detail string) (string, error) {
	// The mission is read for ATTRIBUTION, not permission: who is asking, on whose
	// behalf, and — the field that matters — which session fired it, so the
	// question can be announced where the operator is actually looking. A mission
	// that cannot be read still asks; the question simply travels with less
	// context.
	var parentSessionID, agentName, intent string
	if a.missions != nil {
		if m, err := a.missions.Get(ctx, missionID); err == nil && m != nil {
			parentSessionID, agentName, intent = m.ParentSessionID, m.AgentName, m.Intent
		}
	}
	return a.hitl.RequestAttention(ctx, hitlservice.AttentionRequest{
		Summary:   summary,
		Detail:    detail,
		MissionID: missionID,
		AgentName: agentName,
		OnRaised: func(askID string) {
			// Announce the question the same way a report is announced — on the bus,
			// self-contained, for whoever is listening. serve's router turns this into
			// the card that appears in the session that fired the mission; with no
			// listener it changes nothing, because the ask is already durable and
			// answerable from the queue.
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
}

// publishAsked emits the attention-asked event, best-effort: a question that was
// recorded but not announced is still answerable from the ask queue, so a publish
// failure must never fail the ask (the same rule missionservice applies to
// ReportAddedEvent).
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

// transportPrompter adapts this process's late-bound transport to the
// session-prompting capability the agent-answer offer needs. Late-bound for the
// same reason the deliverer is: the connection does not exist when the fleet is
// composed.
type transportPrompter struct {
	transport func() *acpsvc.Transport
}

func (p transportPrompter) PromptContenoxSession(ctx context.Context, contenoxSessionID, text string) error {
	t := p.transport()
	if t == nil {
		return acpsvc.ErrSessionNotLive
	}
	return t.PromptContenoxSession(ctx, contenoxSessionID, text)
}

func acpPolicySource() hitlservice.PolicySource {
	home, err := os.UserHomeDir()
	if err != nil {
		return hitlservice.NewFSPolicySource()
	}
	return hitlservice.NewFSPolicySource(filepath.Join(home, ".contenox"))
}

// acpUpdateBanner checks for a newer contenox version and returns a one-line
// banner to surface in the ACP session, or "" when no update is available or
// the user has opted out via `contenox config set update-check false`.
// The check is non-blocking: it waits at most 500 ms so a cache hit is instant
// and a slow network call is silently skipped (will appear next session from cache).
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

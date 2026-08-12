package contenoxcli

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/contenox/contenox/internal/models/modelrepo"
	"github.com/contenox/contenox/internal/services/agentregistryservice"
	"github.com/contenox/contenox/internal/services/missionservice"
	"github.com/contenox/contenox/internal/services/onboarding"
	"github.com/contenox/contenox/internal/services/sessionservice"
	"github.com/contenox/contenox/internal/services/shellsession"
	"github.com/contenox/contenox/internal/store/runtimetypes"
	"github.com/contenox/contenox/internal/surfaces/acpsvc"
	"github.com/contenox/contenox/internal/surfaces/beamtui/app"
	"github.com/contenox/contenox/internal/surfaces/beamtui/comp/fileaddr"
	"github.com/contenox/contenox/internal/surfaces/beamtui/enginebridge"
	"github.com/contenox/contenox/internal/surfaces/beamtui/frame"
	"github.com/contenox/contenox/internal/surfaces/beamtui/style"
	"github.com/contenox/contenox/internal/surfaces/beamtui/term"
	"github.com/contenox/contenox/internal/surfaces/fleetboot"
	libacp "github.com/contenox/contenox/libacp"
	libdb "github.com/contenox/contenox/libdbexec"
	"github.com/contenox/contenox/libtracker"
	"github.com/spf13/cobra"
	xterm "golang.org/x/term"
)

const (
	beamChainFile = chainAgentBeamFilename
	beamChainEnv  = "CONTENOX_BEAM_CHAIN_PATH"

	beamHITLPolicy = "hitl-policy-beam.json"

	acpSessionIdentity = "acp-client"
)

const tuiLong = `
The transcript flows into your terminal's own scrollback (so copy and paste work
exactly as they always have), the composer takes / for commands, ! for a shell
line, and @ to attach a file, and gated tool calls are answered inline with one
keystroke.

The terminal UI drives the same in-process ACP transport an editor does, so its sessions,
slash commands and approval flow are identical to 'contenox acp'.

  ctrl+x ctrl+e   compose the draft in $EDITOR
  ctrl+c          clear the composer, interrupt a turn, then quit
  ?               the full key list (on an empty composer)

Needs a terminal: for scripted or piped use, run 'contenox "your prompt"'.`

var newCmd = &cobra.Command{
	Use:   "new [dir]",
	Args:  cobra.MaximumNArgs(1),
	Short: "Start a new session in the terminal UI.",
	Long: `Start a NEW session in the contenox terminal UI. The transcript begins empty
every time; to carry on where you left off, use 'contenox resume'.
` + tuiLong,
	RunE: func(cmd *cobra.Command, args []string) error { return runBeam(cmd, args, true) },
}

var resumeCmd = &cobra.Command{
	Use:   "resume [dir]",
	Args:  cobra.MaximumNArgs(1),
	Short: "Reopen your last session in the terminal UI.",
	Long: `Reopen the last active session in the contenox terminal UI, with its transcript
replayed into your scrollback. Name a different one with --session. When there
is no session to resume, this starts a fresh one, exactly as 'contenox new'
would.
` + tuiLong,
	RunE: func(cmd *cobra.Command, args []string) error { return runBeam(cmd, args, false) },
}

func init() {
	resumeCmd.Flags().String("session", "", "Open the named session instead of the last active one (see 'contenox resume --session' errors for the known names)")
	for _, c := range []*cobra.Command{newCmd, resumeCmd} {
		c.Flags().Bool("light", false, "Render for a light terminal background (overrides detection)")
		c.Flags().Bool("plain", false, "Drop all color and unicode: ASCII glyphs, no styling")
		addWorkspaceRootFlag(c)
		rootCmd.AddCommand(c)
	}
}

func runBeam(cmd *cobra.Command, args []string, freshSession bool) error {
	errW := cmd.ErrOrStderr()

	// The non-TTY check happens once, here: every component below assumes it.
	if !xterm.IsTerminal(int(os.Stdout.Fd())) {
		return errors.New("the terminal UI needs a terminal — try: contenox \"your prompt\"")
	}

	parentCtx := cmd.Context()
	if parentCtx == nil {
		parentCtx = context.Background()
	}
	// SIGINT only matters pre-raw-mode: once the terminal is raw, ctrl+c arrives as a keystroke, not a signal.
	ctx, stop := signal.NotifyContext(parentCtx, syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Deferred before the engine is built so it runs after engine teardown.
	defer func() { _ = modelrepo.Shutdown() }()

	// Same $HOME/.contenox the ACP runtime reads from; a cwd-walk would diverge from where the loaders resolve.
	contenoxDir, err := globalContenoxDir()
	if err != nil {
		return fmt.Errorf("resolve contenox dir: %w", err)
	}
	workspaceID := ResolveWorkspaceID(contenoxDir)
	if err := writeEmbeddedHITLPolicies(contenoxDir, false); err != nil {
		return fmt.Errorf("seed HITL policy presets: %w", err)
	}
	if err := seedBeamChainIfMissing(contenoxDir); err != nil {
		return fmt.Errorf("seed beam chain preset: %w", err)
	}

	dbPath, err := resolveDBPath(cmd)
	if err != nil {
		return err
	}
	dbCtx := libtracker.WithNewRequestID(ctx)
	db, err := OpenDBAt(dbCtx, dbPath)
	if err != nil {
		return fmt.Errorf("open database %q: %w", dbPath, err)
	}
	defer db.Close()

	closeLogs, err := setupTelemetryLogging(dbCtx, runtimetypes.New(db.WithoutTransaction()), contenoxDir)
	if err != nil {
		// Beam has not taken the terminal yet, so this still reaches stderr.
		warnTelemetryLoggingUnavailable(errW, err)
	} else {
		defer closeLogs()
	}

	// beam's stderr IS the screen it draws into, so slog must move to a file before beam takes the terminal or a log line corrupts the scrollback.
	beamLogPath, beamTracker, closeBeamLog, logErr := redirectBeamLogsToFile(dbPath)
	if logErr != nil {
		fmt.Fprintf(errW, "beam: file logging unavailable, logs stay on stderr: %v\n", logErr)
	} else {
		defer closeBeamLog()
	}

	opts, err := buildRunOpts(cmd, db, contenoxDir)
	if err != nil {
		return err
	}
	// Beta-gated toolsets are skipped: an invisible toolset cannot make an envelope stale.
	policyNotice := stalePolicyNotice(beamHITLPolicy, policyDirs(contenoxDir), betaGatedToolsets(opts.EffectiveOptInBeta))
	if beamTracker == nil {
		beamTracker = libtracker.NoopTracker{}
	}
	opts.EffectiveTracker = beamTracker
	opts.EffectiveDB = dbPath
	// beam ships with the shell on; the tiered hitl-policy gates it instead of the tool's absence. --shell=false is still honored.
	if !cmd.Root().Flags().Changed("shell") {
		opts.EffectiveEnableLocalExec = true
	}
	beamHITL := newHITLService(contenoxDir, runtimetypes.New(db.WithoutTransaction()), beamTracker, beamHITLPolicy)
	opts.EffectiveHITLService = beamHITL

	// Late-bound: the fleet's report deliverer and the HITL gate both need the transport, which doesn't exist until the loopback is built below.
	var bridge *enginebridge.Bridge
	transportOf := func() *acpsvc.Transport { return bridge.Transport() } // nil-safe on a nil *Bridge

	sessionRouter := acpsvc.NewSessionRouter()
	opts.EffectiveAskApproval = routedAskApproval(sessionRouter, transportOf)

	var zeroConfigNotice string

	engine, err := BuildEngine(ctx, db, opts)
	if err != nil {
		return fmt.Errorf("failed to build engine: %w", err)
	}
	// A closure, not `defer engine.Stop()`: the zero-config path below may replace `engine` with a rebuilt one, and a bare method-value defer would capture today's pointer.
	defer func() { engine.Stop() }()

	if !engine.SetupCheck.Ready() {
		decision, zcErr := onboarding.TryZeroConfig(ctx, db, engine.SetupCheck)
		if zcErr != nil {
			reportErr, _, end := engine.Tracker.Start(ctx, "register", "zero_config_ollama")
			reportErr(zcErr)
			end()
		}
		if decision.Fire {
			// The already-built engine resolved against the pre-registration (empty) config, so it must be rebuilt with the persisted defaults.
			engine.Stop()
			opts.EffectiveDefaultModel = decision.Model
			opts.EffectiveDefaultProvider = "ollama"
			opts.EffectiveConfiguredModel = decision.Model
			opts.EffectiveConfiguredProvider = "ollama"
			engine, err = BuildEngine(ctx, db, opts)
			if err != nil {
				return fmt.Errorf("failed to rebuild engine after zero-config ollama setup: %w", err)
			}
			if engine.SetupCheck.Ready() {
				zeroConfigNotice = fmt.Sprintf("connected to local ollama — model %s · change with /model", decision.Model)
			}
		}
	}
	if !engine.SetupCheck.Ready() {
		fmt.Fprintln(errW, "beam cannot start until LLM setup is ready.")
		PrintSetupIssues(errW, engine.SetupCheck)
		fmt.Fprintln(errW, "\nrun: contenox setup")
		return ErrPreflightBlocked
	}

	chains, err := acpsvc.LoadChainRegistryFrom(beamChainFile, beamChainEnv)
	if err != nil {
		return err
	}

	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("resolve working directory: %w", err)
	}
	// Every downstream consumer (session cwd, the `!` shell, @ completion) flows from this one variable.
	if len(args) > 0 {
		dir, err := filepath.Abs(args[0])
		if err != nil {
			return fmt.Errorf("resolve workspace directory %q: %w", args[0], err)
		}
		st, err := os.Stat(dir)
		if err != nil || !st.IsDir() {
			return fmt.Errorf("workspace %q is not a directory", args[0])
		}
		cwd = dir
	}

	// Default root for a client's proposed cwd; a client that sends no workspace (or "/") is rooted here instead of the filesystem root.
	workspaceRoots, err := buildWorkspaceFactory(cmd, cwd, runtimetypes.New(db.WithoutTransaction()))
	if err != nil {
		return err
	}

	// Same SANDBOX_* scrub composition local_shell gets (see sandbox_scrub.go): the "!" PTY is an agent-reachable shell too.
	shellScrub, _, err := resolvedSandboxEnv(db, engine.Tracker, errW)
	if err != nil {
		return fmt.Errorf("resolve sandbox env: %w", err)
	}

	// HITL-exempt by existing precedent.
	shells := shellsession.NewManager(shellsession.Config{
		CwdResolver: func(context.Context) string { return cwd },
		ScrubEnv:    shellScrub,
	})
	defer shells.Shutdown()

	// Skipped when this process is itself a dispatched unit (beamChainEnv set).
	var (
		missionFleet  acpsvc.MissionDispatcher
		missionAgents acpsvc.MissionAgentResolver
	)
	if strings.TrimSpace(os.Getenv(beamChainEnv)) == "" {
		missions := missionservice.New(db, missionservice.WithEventPublisher(missionEventPublisher(ctx, db, engine.Bus, workspaceID, beamTracker,
			buildInProcessTriggerHook(ctx, db, contenoxDir, workspaceID, engine, opts, errW))))
		fleet, agents, stopFleet, buildErr := fleetboot.BuildInProcessFleet(ctx, fleetboot.Deps{
			DB:           db,
			Bus:          engine.Bus,
			Missions:     missions,
			Tracker:      beamTracker,
			Transport:    transportOf,
			HITL:         beamHITL,
			PolicySource: hitlPolicySource(contenoxDir),
			DiscoverAgents: func(dctx context.Context, agents agentregistryservice.Service) {
				// engine.Tracker, not the Noop this fleet is built with: a discovery pass is what --trace shows.
				discoverChainAgents(dctx, agents, contenoxDir, engine.Tracker, opts.EffectiveOptInBeta)
			},
			// The workspace this process's mission publisher stamps; a dispatched unit's own publisher must stamp the same one.
			WorkspaceID:    workspaceID,
			WorkspaceRoots: workspaceRoots,
		})
		if buildErr != nil {
			return buildErr
		}
		// Children die with the parent: mission lifetime ≤ beam's lifetime.
		defer stopFleet()
		missionFleet, missionAgents = fleet, agents
	}

	// Serves this process's own terminal client AND every relay attachment, so WorkspaceRoots is set rather than nil.
	bridge, err = enginebridge.New(ctx, beamBridgeDeps(opts, enginebridge.Deps{
		Engine:         engine,
		DB:             db,
		ChainRegistry:  chains,
		WorkspaceRoots: workspaceRoots,
		ShellSessions:  shells,
		Fleet:          missionFleet,
		Agents:         missionAgents,
		SessionRouter:  sessionRouter,
	}))
	if err != nil {
		return fmt.Errorf("open engine bridge: %w", err)
	}
	// A non-nil Close error means the loopback didn't fully join; exit rather than let teardown pull the bus/database out from under live goroutines.
	defer func() {
		if closeErr := bridge.Close(); closeErr != nil {
			fmt.Fprintf(errW, "beam: %v\n", closeErr)
			os.Exit(1)
		}
	}()

	stopRelay := serveRemoteAttachments(ctx, contenoxDir, bridge.AgentFactory(),
		buildRelayChainTriggers(db, contenoxDir, workspaceID, engine, opts), beamTracker, errW)
	defer stopRelay()

	if _, err := bridge.Initialize(ctx); err != nil {
		return fmt.Errorf("acp handshake: %w", err)
	}

	// Replays the session's transcript as updates from the same event stream a live turn uses; beam has no separate rendering path.
	sessionName, sessionFlag, err := resolveBeamSession(ctx, db, workspaceID, cmd, freshSession)
	if err != nil {
		return err
	}
	fresh := true
	if sessionName != "" {
		if _, err := bridge.LoadSession(ctx, libacp.LoadSessionRequest{
			SessionID: libacp.SessionID(sessionName),
			Cwd:       cwd,
		}); err != nil {
			return fmt.Errorf("load session %q: %w", sessionName, err)
		}
		fresh = sessionFlag.messages == 0
	} else {
		resp, err := bridge.NewSession(ctx, libacp.NewSessionRequest{Cwd: cwd})
		if err != nil {
			return fmt.Errorf("start session: %w", err)
		}
		sessionName = string(resp.SessionID)
	}
	sessionID := libacp.SessionID(sessionName)
	bridge.SetActiveSession(sessionID)

	// Terminal detection runs exactly once; every component below takes the resulting Caps as data.
	caps := style.DetectFromOS(true)
	if light, _ := cmd.Flags().GetBool("light"); light {
		caps.Dark = false
	}
	if plain, _ := cmd.Flags().GetBool("plain"); plain {
		caps.Profile = style.ProfileMono
	}
	// Printed after Caps is final but before term.New enters raw mode: beam has no alt-screen, so this becomes the first content in the real scrollback.
	if zeroConfigNotice != "" || policyNotice != "" || beamLogPath != "" {
		prefix, suffix := style.New(caps).SGR(frame.StyleMuted)
		if zeroConfigNotice != "" {
			fmt.Fprintf(os.Stdout, "%s%s%s\n", prefix, zeroConfigNotice, suffix)
		}
		if policyNotice != "" {
			fmt.Fprintf(os.Stdout, "%s%s%s\n", prefix, policyNotice, suffix)
		}
		if beamLogPath != "" {
			fmt.Fprintf(os.Stdout, "%slogs: %s%s\n", prefix, beamLogPath, suffix)
		}
		fmt.Fprintln(os.Stdout)
	}
	engineTerm, err := term.New(os.Stdin, os.Stdout, style.New(caps))
	if err != nil {
		return err
	}

	files, err := fileaddr.NewSource(nil, cwd)
	if err != nil {
		// A refused cwd is not an error (fileaddr returns a rootless Source); this is the genuinely broken case, and @ is the only casualty.
		fmt.Fprintf(errW, "beam: @ file completion unavailable: %v\n", err)
		files = nil
	}

	return app.Run(ctx, app.Deps{
		Term:         engineTerm,
		Bridge:       bridge,
		Caps:         caps,
		SessionID:    sessionID,
		Cwd:          cwd,
		FreshSession: fresh,
		Model:        opts.EffectiveDefaultModel,
		Provider:     opts.EffectiveDefaultProvider,
		SessionName:  sessionName,
		FileSource:   files,
		Editor: func(seed string) (string, error) {
			return captureFromEditor([]byte(seed), opts.EffectiveDefaultModel)
		},
	})
}

func beamBridgeDeps(opts chatOpts, built enginebridge.Deps) enginebridge.Deps {
	deps := built
	if deps.Engine != nil {
		deps.Bus = deps.Engine.Bus
	}
	deps.Tracker = opts.EffectiveTracker
	deps.DefaultModel = opts.EffectiveDefaultModel
	deps.DefaultProvider = opts.EffectiveDefaultProvider
	deps.DefaultAltModel = opts.EffectiveAltDefaultModel
	deps.DefaultAltProvider = opts.EffectiveAltDefaultProvider
	deps.DefaultMaxTokens = opts.EffectiveMaxTokens
	deps.DefaultThink = opts.EffectiveThink
	deps.WorkspaceID = ResolveWorkspaceID(opts.ContenoxDir)
	deps.ContenoxDir = opts.ContenoxDir
	deps.KnownPolicies = embeddedPolicyNames()
	deps.HITLDefaultPolicyName = beamHITLPolicy
	deps.MissionEnvelopes = newMissionEnvelopes(opts.ContenoxDir)
	deps.OptInBeta = opts.EffectiveOptInBeta
	deps.ClientInfo = &libacp.Implementation{Name: "beam", Version: CLIVersion()}
	if opts.EffectiveHITL {
		deps.Asks = opts.EffectiveHITLService
	}
	return deps
}

const beamLogFileName = "beam.log"

func redirectBeamLogsToFile(dbPath string) (string, libtracker.ActivityTracker, func(), error) {
	logPath := filepath.Join(filepath.Dir(dbPath), beamLogFileName)
	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return "", nil, nil, fmt.Errorf("open %s: %w", logPath, err)
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(f, &slog.HandlerOptions{Level: slog.LevelWarn})))
	// The lifecycle tracker writes Info-level spans to the same file; the Warn floor above only guards stray direct slog records.
	return logPath, libtracker.NewTextActivityTracker(f), func() { _ = f.Close() }, nil
}

type beamSession struct {
	messages int
}

func resolveBeamSession(ctx context.Context, db libdb.DBManager, workspaceID string, cmd *cobra.Command, freshSession bool) (string, beamSession, error) {
	// Empty name is the caller's contract for "mint one"; returning early also means a fresh session costs no query.
	if freshSession {
		return "", beamSession{}, nil
	}

	sessions := sessionservice.New(db, workspaceID, libtracker.NoopTracker{})
	roster, err := sessions.List(ctx, acpSessionIdentity)
	if err != nil {
		return "", beamSession{}, fmt.Errorf("list sessions: %w", err)
	}

	if name, _ := cmd.Flags().GetString("session"); strings.TrimSpace(name) != "" {
		name = strings.TrimSpace(name)
		for _, s := range roster {
			if s.Name == name {
				return s.Name, beamSession{messages: s.MessageCount}, nil
			}
		}
		return "", beamSession{}, fmt.Errorf("session %q not found%s", name, knownSessionHint(roster))
	}

	activeID, err := sessions.GetActiveID(ctx)
	if err != nil || activeID == "" {
		return "", beamSession{}, nil
	}
	for _, s := range roster {
		if s.ID == activeID {
			return s.Name, beamSession{messages: s.MessageCount}, nil
		}
	}
	// The active pointer names a session beam cannot load: start a fresh one rather than fail.
	return "", beamSession{}, nil
}

func knownSessionHint(roster []*sessionservice.SessionInfo) string {
	const maxListed = 8
	if len(roster) == 0 {
		return "\n  no sessions yet — run 'contenox new' to start one"
	}
	var b strings.Builder
	b.WriteString("\n  known sessions:")
	for i, s := range roster {
		if i == maxListed {
			fmt.Fprintf(&b, "\n    … and %d more", len(roster)-maxListed)
			break
		}
		fmt.Fprintf(&b, "\n    %s", s.Name)
	}
	return b.String()
}

// beam_cmd.go is beam's composition root: it opens the DB, builds the
// engine, gates on setup, embeds the mission fleet, opens the in-process ACP
// loopback, resolves the session, and runs the app-shell loop, owning
// teardown of each step it started. It renders nothing itself; UI lives in
// beamtui/app and beamtui/comp.
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

	libdb "github.com/contenox/contenox/internal/libdbexec"
	"github.com/contenox/contenox/internal/libtracker"
	"github.com/contenox/contenox/internal/models/modelrepo"
	"github.com/contenox/contenox/internal/services/agentregistryservice"
	"github.com/contenox/contenox/internal/services/hitlservice"
	"github.com/contenox/contenox/internal/services/localtools"
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
	"github.com/spf13/cobra"
	xterm "golang.org/x/term"
)

const (
	// beamChainFile / beamChainEnv are beam's own defaults, seeded to
	// default-beam-chain.json and overridable independently of `contenox
	// acp`'s CONTENOX_ACP_CHAIN_PATH — the same per-profile pattern
	// acpProfileACP/acpProfileACPX use (acp_cmd.go). beam still drives the
	// same in-process ACP Transport an editor does; only the default chain
	// content and its override var are its own.
	beamChainFile = defaultBeamChainFilename
	beamChainEnv  = "CONTENOX_BEAM_CHAIN_PATH"

	// beamHITLPolicy is beam's own default HITL envelope — an attended
	// coding session, tuned like hitl-policy-acp.json (see hitl-policy-beam.json)
	// — and the policy name beam reports for `/policy`.
	beamHITLPolicy = "hitl-policy-beam.json"

	// acpSessionIdentity is the identity acpsvc stores beam's ACP sessions
	// under (acpsvc/session.go), separate from the CLI's local-user identity.
	acpSessionIdentity = "acp-client"
)

var beamCmd = &cobra.Command{
	Use:   "beam [dir]",
	Args:  cobra.MaximumNArgs(1),
	Short: "The contenox terminal UI.",
	Long: `Run contenox as a terminal UI: the transcript flows into your terminal's own
scrollback (so copy and paste work exactly as they always have), the composer
takes / for commands, ! for a shell line, and @ to attach a file, and gated
tool calls are answered inline with one keystroke.

beam drives the same in-process ACP transport an editor does, so its sessions,
slash commands and approval flow are identical to 'contenox acp'.

  ctrl+x ctrl+e   compose the draft in $EDITOR
  ctrl+c          clear the composer, interrupt a turn, then quit
  ?               the full key list (on an empty composer)

Needs a terminal: for scripted or piped use, run 'contenox "your prompt"'.`,
	RunE: runBeam,
}

func init() {
	beamCmd.Flags().String("session", "", "Open the named session instead of the last active one (see 'contenox beam --session' errors for the known names)")
	beamCmd.Flags().Bool("light", false, "Render for a light terminal background (overrides detection)")
	beamCmd.Flags().Bool("plain", false, "Drop all color and unicode: ASCII glyphs, no styling")
	rootCmd.AddCommand(beamCmd)
}

func runBeam(cmd *cobra.Command, args []string) error {
	errW := cmd.ErrOrStderr()

	// The non-TTY check happens once, here: every component below assumes it.
	if !xterm.IsTerminal(int(os.Stdout.Fd())) {
		return errors.New("beam needs a terminal — try: contenox \"your prompt\"")
	}

	parentCtx := cmd.Context()
	if parentCtx == nil {
		parentCtx = context.Background()
	}
	// SIGINT only matters pre-raw-mode: once the terminal is raw, ctrl+c
	// arrives as a keystroke, not a signal.
	ctx, stop := signal.NotifyContext(parentCtx, syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Deferred before the engine is built so it runs after engine teardown.
	defer func() { _ = modelrepo.Shutdown() }()

	// Same $HOME/.contenox the ACP runtime reads from — a cwd-walk would
	// diverge from where the chain and policy loaders resolve.
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
	// A stale policy falls through unnoticed inside the TUI, so it's named
	// here and printed later beside the "logs:" line.
	policyNotice := stalePolicyNotice(beamHITLPolicy, policyDirs(contenoxDir))

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

	// Unlike every other surface, beam's stderr IS the screen it draws the
	// transcript into, so slog must move to a file (beam.log) before beam
	// takes the terminal, or a log line would corrupt the scrollback.
	beamLogPath, beamTracker, closeBeamLog, logErr := redirectBeamLogsToFile(dbPath)
	if logErr != nil {
		// Not fatal: slog stays on stderr, which is ugly but not silent.
		fmt.Fprintf(errW, "beam: file logging unavailable, logs stay on stderr: %v\n", logErr)
	} else {
		defer closeBeamLog()
	}

	opts, err := buildRunOpts(cmd, db, contenoxDir)
	if err != nil {
		return err
	}
	if beamTracker == nil {
		beamTracker = libtracker.NoopTracker{}
	}
	opts.EffectiveTracker = beamTracker
	opts.EffectiveDB = dbPath
	// beam ships with the shell on, unlike `contenox chat`'s scripted default:
	// the tiered hitl-policy gates it instead of the tool's absence. An
	// explicit `--shell=false` is still honored.
	if !cmd.Root().Flags().Changed("shell") {
		opts.EffectiveEnableLocalExec = true
	}

	// Late-bound: the fleet's report deliverer and the HITL gate both need
	// the transport, which doesn't exist until the loopback is built below.
	var bridge *enginebridge.Bridge
	transportOf := func() *acpsvc.Transport { return bridge.Transport() } // nil-safe on a nil *Bridge

	// Routes through the ACP permission flow so approvals render as the
	// inline card, not the CLI's tty prompt (which would fight raw mode).
	opts.EffectiveAskApproval = localtools.AskApproval(func(ctx context.Context, req hitlservice.ApprovalRequest) (bool, error) {
		t := transportOf()
		if t == nil {
			return false, fmt.Errorf("beam: HITL approval requested before the transport was ready")
		}
		return t.AskApproval(ctx, req)
	})

	// Set only when the zero-config path below fires; printed once.
	var zeroConfigNotice string

	// Built exactly as chat builds it, so beam inherits chat's model
	// resolution, tools and HITL wiring rather than a second setup.
	engine, err := BuildEngine(ctx, db, opts)
	if err != nil {
		return fmt.Errorf("failed to build engine: %w", err)
	}
	// A closure, not `defer engine.Stop()`: the zero-config path below may
	// replace `engine` with a rebuilt one, and a bare method-value defer
	// would capture today's pointer, leaking the replacement.
	defer func() { engine.Stop() }()

	// Zero-keystroke first run: TryZeroConfig only probes on the virgin-install
	// shape and registers nothing unless a chat-capable model is found.
	if !engine.SetupCheck.Ready() {
		decision, zcErr := onboarding.TryZeroConfig(ctx, db, engine.SetupCheck)
		if zcErr != nil {
			// Best-effort: falls through to the same gate a virgin install
			// always got. Recorded via the tracker rather than surfaced,
			// since the operator already gets the setup gate's own text.
			reportErr, _, end := engine.Tracker.Start(ctx, "register", "zero_config_ollama")
			reportErr(zcErr)
			end()
		}
		if decision.Fire {
			// The already-built engine resolved against the pre-registration
			// (empty) config, so it must be rebuilt with the defaults
			// onboarding.Apply just persisted.
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
	// `contenox beam .` (or any directory) sets the workspace, like an
	// editor's "open here" — every downstream consumer (session cwd, the `!`
	// shell, @ completion) flows from this one variable.
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

	// Same SANDBOX_* scrub composition local_shell gets (see sandbox_scrub.go):
	// the "!" PTY is an agent-reachable shell too.
	shellScrub, _, err := resolvedSandboxEnv(db, engine.Tracker, errW)
	if err != nil {
		return fmt.Errorf("resolve sandbox env: %w", err)
	}

	// The warm per-session PTY behind `!`: runs in beam's launch directory and
	// is HITL-exempt by existing precedent.
	shells := shellsession.NewManager(shellsession.Config{
		CwdResolver: func(context.Context) string { return cwd },
		ScrubEnv:    shellScrub,
	})
	defer shells.Shutdown()

	// The in-process mission fleet: a mission fired from beam is a subagent
	// of this process, with reports arriving live in the firing session.
	// Skipped when this process is itself a dispatched unit (beamChainEnv set).
	var (
		missionFleet  acpsvc.MissionDispatcher
		missionAgents acpsvc.MissionAgentResolver
	)
	if strings.TrimSpace(os.Getenv(beamChainEnv)) == "" {
		// BuildEngine mints its own hitlservice internally, so this is a
		// sibling instance over the same store: durable asks are shared, but
		// an in-memory parked waiter is not, so a mission's question is
		// answered through the durable queue.
		missions := missionservice.New(db, missionservice.WithEventPublisher(engine.Bus))
		beamHITL := hitlservice.NewWithDefaultPolicy(
			hitlPolicySource(contenoxDir),
			runtimetypes.LocalTenantID,
			runtimetypes.New(db.WithoutTransaction()),
			beamTracker,
			beamHITLPolicy,
		)
		fleet, agents, stopFleet, buildErr := fleetboot.BuildInProcessFleet(ctx, fleetboot.Deps{
			DB:           db,
			Bus:          engine.Bus,
			Missions:     missions,
			Tracker:      beamTracker,
			Transport:    transportOf,
			HITL:         beamHITL,
			PolicySource: hitlPolicySource(contenoxDir),
			DiscoverAgents: func(dctx context.Context, agents agentregistryservice.Service) {
				// engine.Tracker, not the Noop this fleet is built with: a
				// discovery pass is what `--trace` exists to see.
				discoverChainAgents(dctx, agents, contenoxDir, engine.Tracker)
			},
		})
		if buildErr != nil {
			return buildErr
		}
		// Children die with the parent: mission lifetime ≤ beam's lifetime.
		defer stopFleet()
		missionFleet, missionAgents = fleet, agents
	}

	// The in-process ACP loopback. WorkspaceRoots is nil: beam runs on the
	// operator's own machine and takes an absolute cwd as given.
	bridge, err = enginebridge.New(ctx, enginebridge.Deps{
		Engine:                engine,
		DB:                    db,
		Bus:                   engine.Bus,
		Tracker:               beamTracker,
		ChainRegistry:         chains,
		DefaultModel:          opts.EffectiveDefaultModel,
		DefaultProvider:       opts.EffectiveDefaultProvider,
		DefaultAltModel:       opts.EffectiveAltDefaultModel,
		DefaultAltProvider:    opts.EffectiveAltDefaultProvider,
		DefaultMaxTokens:      opts.EffectiveMaxTokens,
		DefaultThink:          opts.EffectiveThink,
		WorkspaceID:           workspaceID,
		ContenoxDir:           contenoxDir,
		WorkspaceRoots:        nil,
		ShellSessions:         shells,
		KnownPolicies:         embeddedPolicyNames(),
		HITLDefaultPolicyName: beamHITLPolicy,
		Fleet:                 missionFleet,
		Agents:                missionAgents,
		ClientInfo:            &libacp.Implementation{Name: "beam", Version: CLIVersion()},
	})
	if err != nil {
		return fmt.Errorf("open engine bridge: %w", err)
	}
	// A non-nil Close error means the loopback didn't fully join, so
	// goroutines may still touch the bus/database the deferred teardown
	// above would pull out from under them — exit rather than continue.
	defer func() {
		if closeErr := bridge.Close(); closeErr != nil {
			fmt.Fprintf(errW, "beam: %v\n", closeErr)
			os.Exit(1)
		}
	}()

	if _, err := bridge.Initialize(ctx); err != nil {
		return fmt.Errorf("acp handshake: %w", err)
	}

	// Loading replays the session's transcript as updates from the same
	// event stream a live turn uses; beam has no separate rendering path.
	sessionName, sessionFlag, err := resolveBeamSession(ctx, db, workspaceID, cmd)
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

	// Terminal detection runs exactly once; every component below takes the
	// resulting Caps as data.
	caps := style.DetectFromOS(true)
	if light, _ := cmd.Flags().GetBool("light"); light {
		caps.Dark = false
	}
	if plain, _ := cmd.Flags().GetBool("plain"); plain {
		caps.Profile = style.ProfileMono
	}
	// Printed after Caps is final but before term.New enters raw mode: beam
	// has no alt-screen, so this becomes the first content in the real
	// scrollback.
	if zeroConfigNotice != "" || policyNotice != "" || beamLogPath != "" {
		prefix, suffix := style.New(caps).SGR(frame.StyleMuted)
		if zeroConfigNotice != "" {
			fmt.Fprintf(os.Stdout, "%s%s%s\n", prefix, zeroConfigNotice, suffix)
		}
		if policyNotice != "" {
			// One line, not a wall: the toolsets, what happens to them, and the
			// verb that fixes it.
			fmt.Fprintf(os.Stdout, "%s%s%s\n", prefix, policyNotice, suffix)
		}
		if beamLogPath != "" {
			// Warnings now land in a file instead of the screen, so it names
			// itself once, on the way in.
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
		// A refused cwd is not an error (fileaddr returns a rootless Source);
		// this is the genuinely broken case, and @ is the only casualty.
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

// beamLogFileName is the log beam redirects slog into. It sits beside the
// database rather than os.UserCacheDir() so a scratch run (`--db /tmp/x.db`)
// keeps its warnings with its own state, and a default run lands in
// ~/.contenox next to telemetry.log.
const beamLogFileName = "beam.log"

// redirectBeamLogsToFile points the default slog handler at a file next to
// dbPath (level WARN and up) and returns its path plus a closer. The path is
// returned rather than printed here so the caller can style it like every
// other pre-TUI line once terminal capabilities are known.
func redirectBeamLogsToFile(dbPath string) (string, libtracker.ActivityTracker, func(), error) {
	logPath := filepath.Join(filepath.Dir(dbPath), beamLogFileName)
	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return "", nil, nil, fmt.Errorf("open %s: %w", logPath, err)
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(f, &slog.HandlerOptions{Level: slog.LevelWarn})))
	// The lifecycle tracker writes Info-level, redacted spans to the same
	// file; the Warn floor above only guards stray direct slog records.
	return logPath, libtracker.NewTextActivityTracker(f), func() { _ = f.Close() }, nil
}

// beamSession is what session resolution learned about the session beam is
// about to open.
type beamSession struct {
	messages int
}

// resolveBeamSession picks the ACP session beam attaches to, reporting "" when
// a fresh one should be created. --session names a known session by name; an
// unknown name errors and lists the known ones rather than auto-creating.
func resolveBeamSession(ctx context.Context, db libdb.DBManager, workspaceID string, cmd *cobra.Command) (string, beamSession, error) {
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
	// The active pointer names a session beam cannot load (a CLI chat session,
	// or one from another workspace): start a fresh one rather than fail.
	return "", beamSession{}, nil
}

// knownSessionHint lists the sessions --session could have named, newest
// first and bounded, so a typo is answered with the answer instead of a
// second lookup.
func knownSessionHint(roster []*sessionservice.SessionInfo) string {
	const maxListed = 8
	if len(roster) == 0 {
		return "\n  beam has no sessions yet — run 'contenox beam' to start one"
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

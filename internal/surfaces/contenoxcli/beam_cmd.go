// beam_cmd.go is beam's composition root (blueprint beam-tui.md D51): the one
// place that sequences open DB → build engine → gate on setup → embed the
// mission fleet → open the in-process ACP loopback → resolve the session →
// take the terminal → run the app-shell loop, and that owns teardown of each
// step it started.
//
// Everything below the seam is somebody else's: the runtime wiring mirrors
// `contenox acp` (acp_cmd.go, cited at each step), the loopback is
// beamtui/enginebridge, and every pixel is beamtui/app plus the components
// under beamtui/comp. This file constructs and orders; it renders nothing and
// decides no UI behavior.
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

	libdb "github.com/contenox/beam/internal/libdbexec"
	"github.com/contenox/beam/internal/libtracker"
	"github.com/contenox/beam/internal/models/modelrepo"
	"github.com/contenox/beam/internal/services/agentregistryservice"
	"github.com/contenox/beam/internal/services/hitlservice"
	"github.com/contenox/beam/internal/services/localtools"
	"github.com/contenox/beam/internal/services/missionservice"
	"github.com/contenox/beam/internal/services/onboarding"
	"github.com/contenox/beam/internal/services/sessionservice"
	"github.com/contenox/beam/internal/services/shellsession"
	"github.com/contenox/beam/internal/store/runtimetypes"
	"github.com/contenox/beam/internal/surfaces/acpsvc"
	"github.com/contenox/beam/internal/surfaces/beamtui/app"
	"github.com/contenox/beam/internal/surfaces/beamtui/comp/fileaddr"
	"github.com/contenox/beam/internal/surfaces/beamtui/enginebridge"
	"github.com/contenox/beam/internal/surfaces/beamtui/frame"
	"github.com/contenox/beam/internal/surfaces/beamtui/style"
	"github.com/contenox/beam/internal/surfaces/beamtui/term"
	"github.com/contenox/beam/internal/surfaces/fleetboot"
	libacp "github.com/contenox/beam/libacp"
	"github.com/spf13/cobra"
	xterm "golang.org/x/term"
)

const (
	// beamChainFile / beamChainEnv are the ACP profile's chain, deliberately
	// shared with `contenox acp`: beam drives the same Transport an editor
	// does, so it must run the same chain or the two surfaces would diverge
	// on the very first prompt.
	beamChainFile = "default-acp-chain.json"
	beamChainEnv  = "CONTENOX_ACP_CHAIN_PATH"

	// beamHITLPolicy is the policy name beam reports for `/policy`. It is
	// hitlservice's own fallback, which is what BuildEngine's HITL service
	// resolves to (engine.go passes an empty fallback), so the name shown and
	// the policy enforced are the same file.
	beamHITLPolicy = "hitl-policy-default.json"

	// acpSessionIdentity is the identity acpsvc stores its sessions under
	// (acpsvc/session.go). beam's sessions ARE ACP sessions — created and
	// loaded through the same Transport — so its roster lives here, not under
	// the CLI's local-user identity.
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

  ctrl+e   compose the draft in $EDITOR
  ctrl+c   clear the composer, interrupt a turn, then quit
  ?        the full key list (on an empty composer)

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

	// (1) D49 — the non-TTY check lives HERE, at the bootstrap layer, and
	// nowhere else: every component below assumes someone upstream did it.
	if !xterm.IsTerminal(int(os.Stdout.Fd())) {
		return errors.New("beam needs a terminal — try: contenox \"your prompt\"")
	}

	parentCtx := cmd.Context()
	if parentCtx == nil {
		parentCtx = context.Background()
	}
	// SIGINT is included for the pre-raw-mode window only: once the terminal
	// is raw, ctrl+c arrives as a keystroke (D3's three-way policy), never as
	// a signal.
	ctx, stop := signal.NotifyContext(parentCtx, syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Deferred before the engine is built so it runs after engine teardown
	// (LIFO), exactly as acp_cmd.go does: drain model-backend shutdown hooks.
	defer func() { _ = modelrepo.Shutdown() }()

	// (2) Seed and locate the same $HOME/.contenox the ACP runtime reads from
	// (acp_cmd.go's comment on globalContenoxDir explains why the cwd-walk is
	// wrong here: the chain and policy loaders resolve via $HOME).
	contenoxDir, err := globalContenoxDir()
	if err != nil {
		return fmt.Errorf("resolve contenox dir: %w", err)
	}
	workspaceID := ResolveWorkspaceID(contenoxDir)
	if err := writeEmbeddedHITLPolicies(contenoxDir, false); err != nil {
		return fmt.Errorf("seed HITL policy presets: %w", err)
	}
	if err := seedACPChainIfMissing(contenoxDir); err != nil {
		return fmt.Errorf("seed ACP chain preset: %w", err)
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
		slog.Warn("Failed to setup telemetry logging", "error", err)
	} else {
		defer closeLogs()
	}

	// (2a) Get slog OFF stderr — beam is about to take this terminal. Every other
	// surface (chat, acp, serve) keeps stderr: a CLI's stderr is the operator's
	// log, and an editor's stderr is the editor's log pane. beam's stderr is the
	// SCREEN. Left alone, an INFO line from the engine wrote raw wrapped text
	// above the welcome, and a WARN mid-session would interleave with the
	// transcript beam is drawing into the same scrollback.
	//
	// This deliberately supersedes the default handler setupTelemetryLogging just
	// installed, whose whole point is a tee to stderr; the file half of that tee
	// is what beam.log now is. WARN+ because this is a log an operator opens when
	// something looked wrong, not a trace.
	beamLogPath, closeBeamLog, logErr := redirectBeamLogsToFile(dbPath)
	if logErr != nil {
		// Printed, not fatal: losing the log file is worth one line of warning,
		// not a refusal to start. slog stays on stderr in this case, which is
		// ugly but honest — better than silently discarding warnings.
		fmt.Fprintf(errW, "beam: file logging unavailable, logs stay on stderr: %v\n", logErr)
	} else {
		defer closeBeamLog()
	}

	opts, err := buildRunOpts(cmd, db, contenoxDir)
	if err != nil {
		return err
	}
	opts.EffectiveDB = dbPath
	// beam ships with the shell ON. The root --shell flag defaults to off
	// because `contenox chat` is scriptable and a scripted surface must opt in;
	// beam is the interactive one, where the gate is the tiered hitl-policy (a
	// read-only or build verb runs, anything else shows the inline approval
	// card) rather than the absence of the tool. Without this, `!git diff`
	// worked while the AGENT could not run git at all and had to say so.
	//
	// An explicit `--shell=false` is still honored: someone who typed the flag
	// meant it.
	if !cmd.Root().Flags().Changed("shell") {
		opts.EffectiveEnableLocalExec = true
	}

	// The transport is late-bound: the fleet's report deliverer and the HITL
	// gate both need it, and it does not exist until the loopback is built.
	// This is acp_cmd.go's `var transport *acpsvc.Transport` pattern, one
	// indirection further out because the Bridge owns the Transport here.
	var bridge *enginebridge.Bridge
	transportOf := func() *acpsvc.Transport { return bridge.Transport() } // nil-safe on a nil *Bridge

	// HITL routes through the ACP permission flow so approvals render as the
	// inline card, NOT through the CLI's tty prompt (which would read stdin
	// out from under the raw-mode terminal). Same closure as acp_cmd.go's.
	opts.EffectiveAskApproval = localtools.AskApproval(func(ctx context.Context, req hitlservice.ApprovalRequest) (bool, error) {
		t := transportOf()
		if t == nil {
			return false, fmt.Errorf("beam: HITL approval requested before the transport was ready")
		}
		return t.AskApproval(ctx, req)
	})

	// zeroConfigNotice is the one dismissible confirmation line blueprint
	// 4.3 asks for, set only when the zero-config path below actually
	// fires. Printed once, later, right before beam takes the terminal (see
	// the comment at its print site in step (7)).
	var zeroConfigNotice string

	// (2b) The engine, built exactly as chat builds it (execChat → BuildEngine
	// with the resolved chatOpts), so beam inherits chat's model resolution,
	// tools and HITL wiring rather than inventing a second setup.
	engine, err := BuildEngine(ctx, db, opts)
	if err != nil {
		return fmt.Errorf("failed to build engine: %w", err)
	}
	// A closure, not `defer engine.Stop()`: the zero-config path below may
	// replace `engine` with a freshly rebuilt one, and a bare method-value
	// defer would capture today's (first) engine pointer at defer time,
	// leaking it. This closure reads `engine` at call time — i.e. whichever
	// one is live when runBeam returns — so exactly one engine is ever
	// stopped, and it is always the last one built.
	defer func() { engine.Stop() }()

	// (3) The setup gate — and, before giving up, first-run's zero-keystroke
	// path (blueprint beam-tui.md section 4.3): fresh install + local Ollama
	// already serving a chat model lands in the composer with no keystrokes
	// at all. onboarding.TryZeroConfig is itself the gate for whether to
	// touch anything: it only probes when engine.SetupCheck already has the
	// virgin-install shape (no backend, no default model, no default
	// provider — onboarding.IsVirginInstall), and it registers nothing
	// unless that probe reports a chat-capable model. Any other failure
	// shape (broken backend, no default set but a backend exists, etc.)
	// passes straight through untouched to the unchanged gate text below.
	if !engine.SetupCheck.Ready() {
		decision, zcErr := onboarding.TryZeroConfig(ctx, db, engine.SetupCheck)
		if zcErr != nil {
			// Best-effort: a failed zero-config attempt (e.g. the backend
			// row couldn't be written) must not crash beam or change the
			// user-facing story — it just falls through to the same gate
			// text a virgin install always got before this path existed.
			slog.Warn("beam: zero-config local ollama registration failed", "error", zcErr)
		}
		if decision.Fire {
			// The already-built engine resolved its defaults against the
			// pre-registration (empty) config, so it cannot simply be
			// reused: rebuild with the defaults onboarding.Apply just
			// persisted, exactly as if the operator had run `contenox
			// backend add ollama --type ollama` followed by two `contenox
			// config set` calls and restarted beam.
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

	// The warm per-session PTY behind `!` (D9): the operator's own line runs
	// in the project directory beam was launched in, streams back as terminal
	// output, and is HITL-exempt by existing precedent.
	shells := shellsession.NewManager(shellsession.Config{
		CwdResolver: func(context.Context) string { return cwd },
	})
	defer shells.Shutdown()

	// (4) The in-process mission fleet, mirroring acp_cmd.go's editor profile:
	// a mission fired from beam is a subagent of THIS process and its reports
	// come back live into the firing session. Skipped when this process is
	// itself a dispatched unit — the chain env being set is exactly that
	// signal (acp_cmd.go: "a DISPATCHED UNIT must not host its own fleet").
	var (
		missionFleet  acpsvc.MissionDispatcher
		missionAgents acpsvc.MissionAgentResolver
	)
	if strings.TrimSpace(os.Getenv(beamChainEnv)) == "" {
		// The publisher-wired mission store and a durable-ask service over the
		// same $HOME/.contenox database serve reads. NOTE: BuildEngine mints
		// its own hitlservice internally and does not hand it back, so this is
		// a sibling instance over the same store — durable asks are shared,
		// but an in-memory parked waiter is not (acp_cmd.go injects one
		// instance for exactly that reason). Answering a mission's question
		// from beam therefore goes through the durable queue.
		missions := missionservice.New(db, missionservice.WithEventPublisher(engine.Bus))
		beamHITL := hitlservice.NewWithDefaultPolicy(
			hitlPolicySource(contenoxDir),
			runtimetypes.LocalTenantID,
			runtimetypes.New(db.WithoutTransaction()),
			libtracker.NoopTracker{},
			beamHITLPolicy,
		)
		fleet, agents, stopFleet, buildErr := fleetboot.BuildInProcessFleet(ctx, fleetboot.Deps{
			DB:           db,
			Bus:          engine.Bus,
			Missions:     missions,
			Tracker:      libtracker.NoopTracker{},
			Transport:    transportOf,
			HITL:         beamHITL,
			PolicySource: hitlPolicySource(contenoxDir),
			DiscoverAgents: func(dctx context.Context, agents agentregistryservice.Service) {
				discoverChainAgents(dctx, agents, contenoxDir)
			},
		})
		if buildErr != nil {
			return buildErr
		}
		// Children die with the parent: mission lifetime ≤ beam's lifetime.
		defer stopFleet()
		missionFleet, missionAgents = fleet, agents
	}

	// (5) The in-process ACP loopback. Deps mirror acp_cmd.go's transport
	// construction — same chain registry, same workspace, same policy display,
	// same fleet — plus the two a TUI owns that a stdio editor does not:
	// WorkspaceRoots (nil: beam runs on the operator's own machine and owns
	// its filesystem, so an absolute cwd is taken as given) and ShellSessions.
	bridge, err = enginebridge.New(ctx, enginebridge.Deps{
		Engine:                engine,
		DB:                    db,
		Bus:                   engine.Bus,
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
	// Close's error is a verdict on what this function may do next: non-nil
	// means the loopback did not fully join, so goroutines may still be
	// touching the bus and the database — and the deferred engine.Stop()
	// (which closes the bus) and db.Close() above would pull them out from
	// under those goroutines. There is nothing safe left to do but leave, so
	// this exits the process rather than running the remaining teardown. The
	// app-shell has already restored the terminal by then.
	defer func() {
		if closeErr := bridge.Close(); closeErr != nil {
			fmt.Fprintf(errW, "beam: %v\n", closeErr)
			os.Exit(1)
		}
	}()

	if _, err := bridge.Initialize(ctx); err != nil {
		return fmt.Errorf("acp handshake: %w", err)
	}

	// (6) The session. Loading replays its transcript as session updates, so
	// the transcript hydrates from the same event stream a live turn uses —
	// beam has no separate history-rendering path.
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

	// (7) The terminal. Detection runs exactly once, here, and every component
	// below takes the resulting Caps as data.
	caps := style.DetectFromOS(true)
	if light, _ := cmd.Flags().GetBool("light"); light {
		caps.Dark = false
	}
	if plain, _ := cmd.Flags().GetBool("plain"); plain {
		caps.Profile = style.ProfileMono
	}
	// Both pre-TUI notices are printed here — after Caps is final, but strictly
	// before term.New puts the terminal into raw mode — because beam has no
	// alt-screen (this command's own help text: "the transcript flows into your
	// terminal's own scrollback"). A line written now becomes literally the first
	// content in that real scrollback: "queued into the first scrollback" for a
	// program with no separate scrollback buffer of its own to queue it into.
	// StyleMuted is resolved the same way every other span in beam is
	// (style.Styles.SGR) rather than an ad hoc escape code, so it matches the dim
	// chrome role used everywhere else once beam is actually running.
	if zeroConfigNotice != "" || beamLogPath != "" {
		prefix, suffix := style.New(caps).SGR(frame.StyleMuted)
		if zeroConfigNotice != "" {
			fmt.Fprintf(os.Stdout, "%s%s%s\n", prefix, zeroConfigNotice, suffix)
		}
		if beamLogPath != "" {
			// Discoverability is the whole reason this line exists: warnings that
			// used to land in front of the operator now land in a file, so the
			// file has to name itself once, on the way in.
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
			// The draft goes IN as the seed and the saved text comes BACK —
			// the two prior-art regressions the blueprint names by hand.
			return captureFromEditor([]byte(seed), opts.EffectiveDefaultModel)
		},
	})
}

// beamLogFileName is the log beam redirects slog into. It sits BESIDE THE
// DATABASE rather than in os.UserCacheDir(): the database path is the one
// location beam already resolves per-invocation and already honors `--db`, so a
// scratch run (`contenox beam --db /tmp/x/scratch.db`) writes its warnings
// beside its own state instead of polluting the shared cache, and a default run
// lands in ~/.contenox next to telemetry.log — the directory an operator
// already looks in.
const beamLogFileName = "beam.log"

// redirectBeamLogsToFile points the default slog handler at a file next to
// dbPath and returns that file's path (for the one muted "logs:" line beam
// prints on the way in) plus a closer. Level is WARN and above.
//
// Returning the path rather than printing it here keeps this a plumbing
// function: the print has to happen later, after terminal capability detection,
// so it can be styled like every other pre-TUI line (see its call site).
func redirectBeamLogsToFile(dbPath string) (string, func(), error) {
	logPath := filepath.Join(filepath.Dir(dbPath), beamLogFileName)
	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return "", nil, fmt.Errorf("open %s: %w", logPath, err)
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(f, &slog.HandlerOptions{Level: slog.LevelWarn})))
	return logPath, func() { _ = f.Close() }, nil
}

// beamSession is what session resolution learned about the session beam is
// about to open.
type beamSession struct {
	messages int
}

// resolveBeamSession picks the ACP session beam attaches to, and reports ""
// when a fresh one should be created.
//
// beam's sessions are ACP sessions: acpsvc mints them under the "acp-client"
// identity with the ACP session id stored as the message-index NAME (see the
// comment in acpsvc's ListSessions, and agentservice.SessionLoad, which
// resolves a load BY NAME). The workspace's active-session pointer holds the
// contenox id, so the name is looked up through sessionservice rather than
// guessed.
//
// --session names one of those sessions. An unknown name is an error that
// lists the known ones (D16: error-and-suggest, not silent auto-create).
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

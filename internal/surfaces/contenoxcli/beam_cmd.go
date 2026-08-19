package contenoxcli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/contenox/contenox/internal/services/operatorinbox"
	"github.com/contenox/contenox/internal/services/presence"
	"github.com/contenox/contenox/internal/surfaces/acpsvc"
	"github.com/contenox/contenox/internal/surfaces/beam/app"
	"github.com/contenox/contenox/internal/surfaces/beam/comp/fileaddr"
	"github.com/contenox/contenox/internal/surfaces/beam/enginebridge"
	"github.com/contenox/contenox/internal/surfaces/beam/frame"
	"github.com/contenox/contenox/internal/surfaces/beam/style"
	"github.com/contenox/contenox/internal/surfaces/beam/term"
	"github.com/contenox/contenox/libacp"
	"github.com/contenox/contenox/libbus"
	"github.com/contenox/contenox/liblog"
	"github.com/spf13/cobra"
	xterm "golang.org/x/term"
)

var beamCmd = &cobra.Command{
	Use:   "beam [path]",
	Short: "Work with contenox in your terminal.",
	Long: `Run contenox as a terminal UI, attached to a runtime this process hosts.

The transcript flows into your terminal's own scrollback, so copy and paste work
exactly as they always have. The composer takes / for commands, ! for a shell
line and @ to attach a file, and a gated tool call is answered inline with one
keystroke.

beam drives the same ACP transport an editor drives, so its sessions, slash
commands and approval flow are identical to 'contenox acp'. With no arguments it
opens the newest session rooted in the current directory, or starts a fresh one
when there is none; a path opens that directory instead.

` + toolGrantLine + `

` + askWaitLine + ` An ask is a durable row before the card
appears and the turn waits on that row, so answering the card continues the turn
right here — and the same ask stays answerable from another terminal or a phone,
or expires on its own if you walk away. Quit with an ask still open and the turn
checkpoints beside it, so answering it later, from anywhere, picks the run up
where it stopped. A card that outlives its turn stays answerable and says so:
its key line reads 'answering resumes the run' instead of offering an Esc that
would have no turn left to cancel.

  ctrl+x ctrl+e   compose the draft in $EDITOR
  ctrl+s          switch sessions
  ctrl+c          clear the composer, interrupt a turn, then quit
  ?               the full key list (on an empty composer)

Logs go to <data-dir>/logs rather than the screen: beam's stderr is the
transcript. If that log cannot be opened, beam says so before it starts and then
runs without logs, rather than writing them over the transcript. For scripted or
piped use run 'contenox run "<task>"' instead.

Examples:
  contenox beam            # the directory you are standing in
  contenox beam ~/src/api  # somewhere else
  contenox beam --new      # ignore the newest session and start clean`,
	Args: cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if err := requireBeamTerminal(); err != nil {
			return err
		}
		return runACPProfile(cmd, acpProfileBeam)
	},
}

func requireBeamTerminal() error {
	if xterm.IsTerminal(int(os.Stdout.Fd())) {
		return nil
	}
	return errors.New("beam needs a terminal — for scripted use run: contenox run \"<task>\"")
}

func init() {
	beamCmd.Flags().String("log-dir", "", "Write beam's logs here (default: <data-dir>/"+hostLogDirName+")")
	beamCmd.Flags().String("session", "", "Open this session id instead of the newest one in the directory")
	beamCmd.Flags().Bool("new", false, "Start a fresh session instead of reopening the newest one")
	beamCmd.Flags().Bool("light", false, "Render for a light terminal background (overrides detection)")
	beamCmd.Flags().Bool("plain", false, "Drop all colour and unicode: ASCII glyphs, no styling")
	registerHITLPolicyFlag(beamCmd)
	rootCmd.AddCommand(beamCmd)
}

const beamAgentJoinTimeout = 10 * time.Second

const beamTransportCloseTimeout = 10 * time.Second

func surfaceLogPath(w *liblog.Writer) string {
	if w == nil {
		return ""
	}
	return w.Path()
}

func beamRoot(cmd *cobra.Command, launchDir string) (string, error) {
	args := cmd.Flags().Args()
	if len(args) == 0 || strings.TrimSpace(args[0]) == "" {
		return launchDir, nil
	}
	root, err := filepath.Abs(args[0])
	if err != nil {
		return "", fmt.Errorf("resolve workspace directory %q: %w", args[0], err)
	}
	info, err := os.Stat(root)
	if err != nil || !info.IsDir() {
		return "", fmt.Errorf("workspace %q is not a directory", args[0])
	}
	return filepath.Clean(root), nil
}

type beamSurface struct {
	factory       libacp.AgentFactory
	bindTransport func(*acpsvc.Transport)
	reporter      *presence.Reporter
	bus           libbus.Messenger
	root          string
	model         string
	provider      string
	engineReady   bool
	logPath       string
	// workspaceEnv scrubs a launched terminal's parent environment (the shell
	// scrub from resolvedSandboxEnv). Nil defers to the shared server's
	// ScrubDenySecrets default rather than the raw os.Environ().
	workspaceEnv func([]string) []string
}

const beamInboxQueueDepth = 16

func watchOperatorInbox(ctx context.Context, bus libbus.Messenger, errW io.Writer) (<-chan []byte, func()) {
	if bus == nil {
		return nil, func() {}
	}
	ch := make(chan []byte, beamInboxQueueDepth)
	sub, err := bus.Stream(ctx, operatorinbox.AddedSubject, ch)
	if err != nil {
		fmt.Fprintf(errW, "beam: operator inbox notices unavailable: %v\n", err)
		return nil, func() {}
	}
	return ch, func() { _ = sub.Unsubscribe() }
}

type duplexPipe struct {
	r *io.PipeReader
	w *io.PipeWriter
}

func (p *duplexPipe) Read(b []byte) (int, error)  { return p.r.Read(b) }
func (p *duplexPipe) Write(b []byte) (int, error) { return p.w.Write(b) }
func (p *duplexPipe) Close() error {
	_ = p.r.Close()
	return p.w.Close()
}

func runBeamSurface(ctx context.Context, cmd *cobra.Command, s beamSurface) error {
	errW := cmd.ErrOrStderr()
	if err := requireBeamTerminal(); err != nil {
		return err
	}
	if !s.engineReady {
		fmt.Fprintln(errW, "beam cannot start until LLM setup is ready.")
		fmt.Fprintln(errW, "\nrun: contenox setup")
		return ErrPreflightBlocked
	}

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	agentR, clientW := io.Pipe()
	clientR, agentW := io.Pipe()
	agentSide := &duplexPipe{r: agentR, w: agentW}
	clientSide := &duplexPipe{r: clientR, w: clientW}

	var transport *acpsvc.Transport
	agentConn := libacp.NewAgentSideConnection(agentSide, func(c *libacp.AgentSideConnection) libacp.Agent {
		agent := s.factory(c)
		transport, _ = agent.(*acpsvc.Transport)
		if s.bindTransport != nil {
			s.bindTransport(transport)
		}
		if s.reporter != nil {
			return newPresenceAgent(agent, s.reporter)
		}
		return agent
	})
	if transport == nil {
		_ = agentSide.Close()
		_ = clientSide.Close()
		return errors.New("beam: the ACP factory did not produce a transport")
	}

	agentDone := make(chan error, 1)
	go func() { agentDone <- agentConn.Run(runCtx) }()

	inbox, stopInbox := watchOperatorInbox(runCtx, s.bus, errW)
	defer stopInbox()

	bridge, err := enginebridge.New(runCtx, enginebridge.Deps{
		Conn:          clientSide,
		ClientInfo:    &libacp.Implementation{Name: "beam", Version: CLIVersion()},
		Inbox:         inbox,
		WorkspaceRoot: s.root,
		WorkspaceEnv:  s.workspaceEnv,
	})
	if err != nil {
		return fmt.Errorf("open engine bridge: %w", err)
	}

	runErr := driveBeam(runCtx, cmd, s, bridge)

	if closeErr := bridge.Close(); closeErr != nil {
		runErr = errors.Join(runErr, fmt.Errorf("close engine bridge: %w", closeErr))
	}

	closeCtx, closeCancel := context.WithTimeout(context.Background(), beamTransportCloseTimeout)
	defer closeCancel()
	if closeErr := transport.Close(closeCtx); closeErr != nil {
		runErr = errors.Join(runErr, fmt.Errorf("close transport: %w", closeErr))
	}

	cancel()
	select {
	case <-agentDone:
	case <-time.After(beamAgentJoinTimeout):
	}
	return runErr
}

func driveBeam(ctx context.Context, cmd *cobra.Command, s beamSurface, bridge *enginebridge.Bridge) error {
	errW := cmd.ErrOrStderr()

	if _, err := bridge.Initialize(ctx); err != nil {
		return fmt.Errorf("acp handshake: %w", err)
	}

	sessionID, fresh, err := resolveBeamSession(ctx, cmd, bridge, s.root)
	if err != nil {
		return err
	}
	bridge.SetActiveSession(sessionID)

	caps := style.DetectFromOS(true)
	if light, _ := cmd.Flags().GetBool("light"); light {
		caps.Dark = false
	}
	if plain, _ := cmd.Flags().GetBool("plain"); plain {
		caps.Profile = style.ProfileMono
	}

	if s.logPath != "" {
		prefix, suffix := style.New(caps).SGR(frame.StyleMuted)
		fmt.Fprintf(os.Stdout, "%slogs: %s%s\n\n", prefix, s.logPath, suffix)
	}

	engineTerm, err := term.New(os.Stdin, os.Stdout, style.New(caps))
	if err != nil {
		return err
	}

	files, err := fileaddr.NewSource(nil, s.root)
	if err != nil {
		fmt.Fprintf(errW, "beam: @ file completion unavailable: %v\n", err)
		files = nil
	}

	return app.Run(ctx, app.Deps{
		Term:         engineTerm,
		Bridge:       bridge,
		Caps:         caps,
		SessionID:    sessionID,
		Cwd:          s.root,
		FreshSession: fresh,
		Model:        s.model,
		Provider:     s.provider,
		SessionName:  string(sessionID),
		FileSource:   files,
		Editor: func(seed string) (string, error) {
			return captureFromEditor([]byte(seed), s.model)
		},
	})
}

func resolveBeamSession(ctx context.Context, cmd *cobra.Command, bridge *enginebridge.Bridge, root string) (libacp.SessionID, bool, error) {
	if named, _ := cmd.Flags().GetString("session"); strings.TrimSpace(named) != "" {
		id := libacp.SessionID(strings.TrimSpace(named))
		if _, err := bridge.LoadSession(ctx, libacp.LoadSessionRequest{SessionID: id, Cwd: root}); err != nil {
			return "", false, fmt.Errorf("load session %q: %w", id, err)
		}
		return id, false, nil
	}

	if startNew, _ := cmd.Flags().GetBool("new"); !startNew {
		if id, ok := newestBeamSession(ctx, bridge, root); ok {
			if _, err := bridge.LoadSession(ctx, libacp.LoadSessionRequest{SessionID: id, Cwd: root}); err == nil {
				return id, false, nil
			}
		}
	}

	resp, err := bridge.NewSession(ctx, libacp.NewSessionRequest{Cwd: root})
	if err != nil {
		return "", false, fmt.Errorf("start session: %w", err)
	}
	return resp.SessionID, true, nil
}

func newestBeamSession(ctx context.Context, bridge *enginebridge.Bridge, root string) (libacp.SessionID, bool) {
	resp, err := bridge.ListSessions(ctx, libacp.ListSessionsRequest{Cwd: root})
	if err != nil || len(resp.Sessions) == 0 {
		return "", false
	}
	return resp.Sessions[0].SessionID, true
}

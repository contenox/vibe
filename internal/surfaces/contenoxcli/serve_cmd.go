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

	"github.com/contenox/contenox/internal/kernel/enginesvc"
	"github.com/contenox/contenox/internal/relaycreds"
	"github.com/contenox/contenox/internal/relaypair"
	"github.com/contenox/contenox/internal/services/setupcheck"
	"github.com/contenox/contenox/internal/store/runtimetypes"
	"github.com/contenox/contenox/internal/surfaces/contenoxcli/brand"
	"github.com/contenox/contenox/liblog"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

var serveCmd = &cobra.Command{
	Use:   "serve [path]",
	Short: "Keep contenox running so the app can reach this machine.",
	Long: `Run contenox as a host: the full runtime, reachable from the contenox app
through the relay, with no editor and nobody at a keyboard.

A host is an organization's shape, so it holds no capability of its own:
local_fs and local_shell are not mounted at all, and every tool it can reach is
an MCP server you registered ('contenox mcp list'). A declaration that asks for
a file read or a shell line is refused here by shape, not by policy — give it an
MCP tool instead. For an agent that needs your files and your shell, run
'contenox beam' or attach an ACP editor.

` + toolGrantLine + ` What a host grants is bounded by
shape first: a toolset it does not mount cannot be admitted by any of them.

` + askWaitLine + ` Nobody is at this keyboard, so
answer them with 'contenox approvals respond'.

One instance serves exactly ONE workspace, fixed when it launches; a client that
needs a different one attaches to a different instance. With a path, that
directory is the workspace for sessions the app opens. With no path the host
serves your home directory, because a host is a property of the machine rather
than of the shell it was started from — use 'contenox serve .' to scope it to
the current directory instead.

The host checks its own setup before it starts serving, prints what it is
attached to, and then stays up until interrupted.

Structured logs go to <data-dir>/logs rather than the screen, named by date as
serve-<YYYY-MM-DD>.log; a day that outgrows its size bound continues in a
numbered part. Their bounds are configuration, not flags:

  contenox config set log-max-size 50MB      # new part at this size
  contenox config set log-max-files 4        # kept across every date and part
  contenox config set log-max-age-days 7     # retired by date

Examples:
  contenox serve            # host, rooted at your home directory
  contenox serve .          # host, scoped to the current directory
  contenox serve ~/src/api  # host, scoped to one workspace`,
	Args: cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return runACPProfile(cmd, acpProfileServe)
	},
}

const hostLogDirName = "logs"

func init() {
	serveCmd.Flags().String("log-dir", "", "Write host logs here (default: <data-dir>/"+hostLogDirName+")")
	registerHITLPolicyFlag(serveCmd)
	rootCmd.AddCommand(serveCmd)
}

func openHostLog(cmd *cobra.Command, name string) (*liblog.Writer, error) {
	dir, _ := cmd.Flags().GetString("log-dir")
	if strings.TrimSpace(dir) == "" {
		base, err := globalContenoxDir()
		if err != nil {
			return nil, err
		}
		dir = filepath.Join(base, hostLogDirName)
	}
	return liblog.Open(liblog.Config{Dir: dir, Name: name})
}

// applyStoredLogSettings moves a live host log onto the operator's configured
// bounds; anything unset stays on the default it booted with.
func applyStoredLogSettings(ctx context.Context, store runtimetypes.Store, w *liblog.Writer) {
	if w == nil {
		return
	}
	w.Reconfigure(logSettingsFromConfig(ctx, store))
}

// defaultHostRoot resolves the workspace root a host serves: with no path, the
// user's home directory rather than the launch directory, since a host outlives
// the shell that started it. A relative path is made absolute.
func defaultHostRoot(cmd *cobra.Command, launchDir string) string {
	args := cmd.Flags().Args()
	if len(args) == 0 || strings.TrimSpace(args[0]) == "" {
		if home, err := os.UserHomeDir(); err == nil && home != "" {
			return filepath.Clean(home)
		}
		// No home to serve: better the directory we are in than nothing.
		return launchDir
	}
	root := args[0]
	if !filepath.IsAbs(root) {
		if abs, err := filepath.Abs(root); err == nil {
			root = abs
		}
	}
	return filepath.Clean(root)
}

// hostSetupCheck lifts the readiness result off the engine the host already
// built; a setup-only host has none.
func hostSetupCheck(_ context.Context, engine *enginesvc.Engine) *setupcheck.Result {
	if engine == nil {
		return nil
	}
	res := engine.SetupCheck
	return &res
}

// relayIsConfigured reports whether this machine has stored relay credentials.
func relayIsConfigured(contenoxDir string) bool {
	_, err := relaycreds.Load(contenoxDir)
	return err == nil
}

// hostScreen is everything the status display names.
type hostScreen struct {
	contenoxDir  string
	workspaceID  string
	root         string
	model        string
	provider     string
	engineReady  bool
	setupCheck   *setupcheck.Result
	log          *liblog.Writer
	relayEnabled bool
}

// runHost draws the status screen and blocks until the context is cancelled.
// The runtime and the relay tunnel are already running, and tearing them down is
// the caller's business.
func runHost(ctx context.Context, cmd *cobra.Command, s hostScreen) error {
	out := cmd.OutOrStdout()
	writeHostScreen(out, s, isTerminal(out))

	<-ctx.Done()
	fmt.Fprintln(out, "\nStopping — the app can no longer reach this machine.")
	return nil
}

// writeHostScreen renders the status display.
func writeHostScreen(w io.Writer, s hostScreen, colour bool) {
	fmt.Fprintln(w)
	brand.WriteHeader(w, brand.Options{Colour: colour})
	fmt.Fprintln(w)

	// Setup first: a host with no usable model is reachable but useless.
	var next []string
	next = append(next, writeHostReadiness(w, s)...)

	fmt.Fprintf(w, "  %-12s %s\n", "Workspace", s.root)
	if s.workspaceID != "" {
		fmt.Fprintf(w, "  %-12s %s\n", "ID", s.workspaceID)
	}
	if s.model != "" {
		model := s.model
		if s.provider != "" {
			model = s.provider + "/" + s.model
		}
		fmt.Fprintf(w, "  %-12s %s\n", "Model", model)
	}
	next = append(next, writeHostRelay(w, s)...)
	writeHostLog(w, s)

	if len(next) > 0 {
		fmt.Fprintln(w)
		for _, line := range next {
			fmt.Fprintf(w, "  %s\n", line)
		}
	}

	fmt.Fprintln(w)
	fmt.Fprintln(w, "  Running. Press Ctrl-C to stop.")
	fmt.Fprintln(w)
}

func writeHostReadiness(w io.Writer, s hostScreen) []string {
	if !s.engineReady {
		fmt.Fprintf(w, "  %-12s no model configured\n", "Setup")
		return []string{
			"This host can be attached to, but nothing it is asked to do will run.",
			"Configure a provider and model:  contenox setup",
		}
	}
	if s.setupCheck == nil {
		return nil
	}
	ready, reason, next := doctorVerdict(*s.setupCheck)
	if ready {
		fmt.Fprintf(w, "  %-12s ready\n", "Setup")
		return nil
	}
	fmt.Fprintf(w, "  %-12s %s\n", "Setup", reason)
	if next != "" {
		return []string{next}
	}
	return nil
}

func writeHostRelay(w io.Writer, s hostScreen) []string {
	creds, err := relaycreds.Load(s.contenoxDir)
	if err != nil {
		if errors.Is(err, relaycreds.ErrNotEnrolled) {
			fmt.Fprintf(w, "  %-12s not paired — reachable on this machine only\n", "Relay")
			return []string{
				"To reach this host from the contenox app:",
				fmt.Sprintf("  1. Sign in at %s and tap Pair device", relaypair.DefaultAppEndpoint),
				"  2. contenox pair <key>",
				"  3. restart this host",
			}
		}
		fmt.Fprintf(w, "  %-12s stored pairing unreadable: %v\n", "Relay", err)
		return []string{"Clear the unreadable pairing:  contenox unpair"}
	}
	fmt.Fprintf(w, "  %-12s attached to %s\n", "Relay", creds.Endpoint)
	fmt.Fprintf(w, "  %-12s %s\n", "Instance", creds.InstanceID)
	if origin, oerr := relaypair.AppOrigin(creds.Endpoint); oerr == nil {
		fmt.Fprintf(w, "  %-12s %s\n", "App", origin)
	}
	return nil
}

func writeHostLog(w io.Writer, s hostScreen) {
	if s.log == nil {
		fmt.Fprintf(w, "  %-12s stderr (no log file)\n", "Logs")
		return
	}
	fmt.Fprintf(w, "  %-12s %s\n", "Logs", s.log.Path())
	fmt.Fprintf(w, "  %-12s new part at %s · keep %s · %s\n", "",
		liblog.FormatSize(s.log.MaxBytes()), keepDescription(s.log.MaxFiles()), ageDescription(s.log.MaxAge()))
	fmt.Fprintf(w, "  %-12s contenox config set log-max-size 50MB\n", "")
}

func keepDescription(n int) string {
	if n <= 0 {
		return "every file"
	}
	return fmt.Sprintf("%d files", n)
}

func ageDescription(d time.Duration) string {
	if d <= 0 {
		return "no age limit"
	}
	days := int(d.Hours() / 24)
	if days < 1 {
		return d.String()
	}
	if days == 1 {
		return "1 day"
	}
	return fmt.Sprintf("%d days", days)
}

// isTerminal reports whether w is a terminal, which decides colour.
func isTerminal(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	return term.IsTerminal(int(f.Fd()))
}

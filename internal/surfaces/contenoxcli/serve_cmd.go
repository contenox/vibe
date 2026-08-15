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

// serve keeps a contenox runtime up so the hosted app can attach to this
// machine. Without it the app is only reachable while an editor happens to be
// running an ACP session, which makes "open contenox on your phone" depend on
// a program that has nothing to do with the relay.
//
// beam is the same host under the name the terminal UI used, kept so the
// muscle memory keeps working.
var serveCmd = &cobra.Command{
	Use:   "serve [path]",
	Short: "Keep contenox running so the app can reach this machine.",
	Long: `Run contenox as a host: the full runtime, reachable from the contenox app
through the relay, with no editor involved.

With a path, that directory is the default workspace root for sessions the app
opens. With no path the host serves your home directory, because a host is a
property of the machine rather than of the shell it was started from — use
'contenox serve .' to scope it to the current directory instead.

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

var beamCmd = &cobra.Command{
	Use:    "beam [path]",
	Short:  "Alias for 'contenox serve'.",
	Long:   "Alias for 'contenox serve': keeps contenox running so the app can reach this machine.",
	Args:   cobra.MaximumNArgs(1),
	Hidden: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		return runACPProfile(cmd, acpProfileServe)
	},
}

// hostLogName is the base name of the host's own logs, kept separate from
// telemetry.log so turning telemetry on or off never changes whether a host
// has a log. Files land as serve-<date>.log inside hostLogDirName.
const (
	hostLogName    = "serve"
	hostLogDirName = "logs"
)

func init() {
	for _, c := range []*cobra.Command{serveCmd, beamCmd} {
		c.Flags().String("log-dir", "", "Write host logs here (default: <data-dir>/"+hostLogDirName+")")
		rootCmd.AddCommand(c)
	}
}

// openHostLog opens the host's log directory. It runs before the database is
// readable, so it starts on defaults; the stored `log-*` settings are applied
// by [applyStoredLogSettings] once the database is open. A log that only works
// after the database opens is no use for diagnosing a database that will not.
func openHostLog(cmd *cobra.Command) (*liblog.Writer, error) {
	dir, _ := cmd.Flags().GetString("log-dir")
	if strings.TrimSpace(dir) == "" {
		base, err := globalContenoxDir()
		if err != nil {
			return nil, err
		}
		dir = filepath.Join(base, hostLogDirName)
	}
	return liblog.Open(liblog.Config{Dir: dir, Name: hostLogName})
}

// applyStoredLogSettings moves a live host log onto the operator's configured
// bounds. Anything unset stays on the default it booted with.
func applyStoredLogSettings(ctx context.Context, store runtimetypes.Store, w *liblog.Writer) {
	if w == nil {
		return
	}
	w.Reconfigure(logSettingsFromConfig(ctx, store))
}

// defaultHostRoot resolves the workspace root a host serves.
//
// With no path, that is the user's home directory, not the directory the host
// was started in. A host is a property of the machine — it outlives the shell
// that launched it and is reached from a phone that knows nothing about where
// that shell was standing — so scoping it to a happenstance working directory
// would make what the app can open depend on an invisible detail. `serve .`
// is how you ask for the narrow scope, and it says so out loud.
//
// A relative path is made absolute here so the screen names the same directory
// the sessions actually get.
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
// built, so the pre-flight costs nothing extra. A host serving setup-only has
// no engine and therefore no result.
func hostSetupCheck(_ context.Context, engine *enginesvc.Engine) *setupcheck.Result {
	if engine == nil {
		return nil
	}
	res := engine.SetupCheck
	return &res
}

// relayIsConfigured reports whether this machine has stored relay credentials
// at all, which is what decides between "attached" and "pair me" on screen.
func relayIsConfigured(contenoxDir string) bool {
	_, err := relaycreds.Load(contenoxDir)
	return err == nil
}

// hostScreen is everything the status display names. It is a plain value so
// the renderer is testable without a running runtime.
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

// runHost draws the status screen and blocks until the context is cancelled,
// which is what SIGINT/SIGTERM resolve to. The runtime and the relay tunnel
// are already running by the time this is called; tearing them down is the
// caller's deferred business.
func runHost(ctx context.Context, cmd *cobra.Command, s hostScreen) error {
	out := cmd.OutOrStdout()
	writeHostScreen(out, s, isTerminal(out))

	<-ctx.Done()
	fmt.Fprintln(out, "\nStopping — the app can no longer reach this machine.")
	return nil
}

// writeHostScreen renders the status display. Everything it prints is a fact
// about this process; nothing is aspirational, because a host that claims to
// be reachable when it is not is worse than one that says nothing.
func writeHostScreen(w io.Writer, s hostScreen, colour bool) {
	fmt.Fprintln(w)
	brand.WriteHeader(w, brand.Options{Colour: colour})
	fmt.Fprintln(w)

	// Setup first: a host with no usable model can still be reached, but
	// nothing it is asked to do will work, and that is worth saying before
	// the reassuring lines below.
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

	// Every instruction lands here rather than between the status rows, so the
	// aligned block stays scannable and the things to do stay together.
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

// writeHostReadiness prints the setup row and returns any follow-up lines for
// the instructions block.
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

// writeHostRelay prints the relay rows and returns any follow-up lines. The
// unpaired case is the whole reason this screen exists, so it gets the full
// three-step funnel rather than a terse hint.
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

// keepDescription and ageDescription render a retention bound the way the
// config key that sets it reads, including the operator's 0 for "no limit".
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

// isTerminal reports whether w is a terminal, which decides colour. A
// redirected stdout gets plain text so a captured screen stays readable.
func isTerminal(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	return term.IsTerminal(int(f.Fd()))
}

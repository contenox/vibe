package contenoxcli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/contenox/contenox/internal/relaycreds"
	"github.com/contenox/contenox/liblog"
	"github.com/spf13/cobra"
)

// serve and beam must be reserved, or `contenox serve` with a typo'd flag
// would be injected as a chat prompt and spend a model call saying nothing.
func TestUnit_serveAndBeamAreReservedSubcommands(t *testing.T) {
	for _, name := range []string{"serve", "beam"} {
		if !reservedSubcommands[name] {
			t.Fatalf("%q must be reserved", name)
		}
		if !firstNonFlagIsReserved([]string{name}) {
			t.Fatalf("expected %q to be recognized as a reserved subcommand", name)
		}
	}
}

// The bug this fixes: `contenox pair HWV-XT8` used to fall through to the chat
// chain and send a short-lived pairing credential to the model provider as
// prompt text. Reserving the name is what stops that.
func TestUnit_pairAndUnpairAreReservedSubcommands(t *testing.T) {
	for _, name := range []string{"pair", "unpair"} {
		if !reservedSubcommands[name] {
			t.Fatalf("%q must be reserved so a pairing key is never injected as chat input", name)
		}
		if !firstNonFlagIsReserved([]string{name}) {
			t.Fatalf("expected %q to be recognized as a reserved subcommand", name)
		}
	}
}

func hostRootFor(t *testing.T, args ...string) string {
	t.Helper()
	cmd := &cobra.Command{Use: "serve", Args: cobra.MaximumNArgs(1), RunE: func(*cobra.Command, []string) error { return nil }}
	cmd.SetArgs(args)
	cmd.SetOut(new(strings.Builder))
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	return defaultHostRoot(cmd, "/launch/dir")
}

// A host outlives the shell that started it and is reached from a device that
// knows nothing about that shell's working directory, so a bare `serve` is
// machine-scoped: the home directory, never wherever it happened to be run.
func TestUnit_Host_RootDefaultsToTheHomeDirectory(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("no home directory on this machine: %v", err)
	}
	got := hostRootFor(t)
	if got != filepath.Clean(home) {
		t.Fatalf("no-arg root = %q, want the home directory %q", got, home)
	}
	if got == "/launch/dir" {
		t.Fatal("a bare 'serve' must not inherit the launch directory")
	}
}

// `serve` and `serve .` must differ: the whole point of the dot is asking for
// the narrow scope explicitly. If they resolved the same way the argument
// would be decoration.
func TestUnit_Host_BareAndDotAreDifferentScopes(t *testing.T) {
	if _, err := os.UserHomeDir(); err != nil {
		t.Skipf("no home directory on this machine: %v", err)
	}
	bare := hostRootFor(t)
	dot := hostRootFor(t, ".")
	if bare == dot {
		t.Fatalf("'serve' and 'serve .' both resolved to %q — the dot must scope down", bare)
	}
}

func TestUnit_Host_ExplicitPathWins(t *testing.T) {
	dir := t.TempDir()
	if got := hostRootFor(t, dir); got != filepath.Clean(dir) {
		t.Fatalf("root = %q, want %q", got, dir)
	}
}

// A relative path must be resolved, or the screen would claim to serve "." and
// the sessions would resolve it against a different directory later.
func TestUnit_Host_RelativePathIsMadeAbsolute(t *testing.T) {
	got := hostRootFor(t, ".")
	if !filepath.IsAbs(got) {
		t.Fatalf("root = %q, want an absolute path", got)
	}
}

// The screen must name the root the sessions actually get. This regressed
// once: the display called defaultHostRoot while the workspace allowlist was
// still built from the launch directory, so `serve /some/path` printed that
// path while serving the directory it was started in — the display lied.
//
// The check is that the resolved root reaches the workspace factory as its
// default, because that factory is what actually bounds every session and
// relay attachment. Asserting only on the printed string would have passed
// while the bug was live.
func TestUnit_Host_ServedRootReachesTheWorkspaceAllowlist(t *testing.T) {
	dir := t.TempDir()
	root := hostRootFor(t, dir)

	factory, err := buildWorkspaceFactory(nil, root, nil)
	if err != nil {
		t.Fatalf("buildWorkspaceFactory: %v", err)
	}
	if got := factory.Default(); got != filepath.Clean(dir) {
		t.Fatalf("workspace default = %q, want the served root %q", got, dir)
	}
	if _, ok := factory.Allows(filepath.Clean(dir)); !ok {
		t.Fatalf("the served root %q is not in the allowlist: %v", dir, factory.Roots())
	}

	out := renderHostScreen(t, hostScreen{contenoxDir: t.TempDir(), root: root, engineReady: true})
	if !strings.Contains(out, filepath.Clean(dir)) {
		t.Fatalf("screen does not name the served root %q:\n%s", dir, out)
	}
}

func renderHostScreen(t *testing.T, s hostScreen) string {
	t.Helper()
	var b strings.Builder
	writeHostScreen(&b, s, false)
	return b.String()
}

// The unpaired screen is the whole point of the funnel: a person who cannot
// reach the app must be told exactly how, in order, without leaving the screen.
func TestUnit_Host_UnpairedScreenFunnelsToPairing(t *testing.T) {
	out := renderHostScreen(t, hostScreen{
		contenoxDir: t.TempDir(),
		root:        "/work",
		engineReady: true,
	})
	for _, want := range []string{"not paired", "Pair device", "contenox pair <key>", "app.contenox.com"} {
		if !strings.Contains(out, want) {
			t.Fatalf("unpaired screen is missing %q:\n%s", want, out)
		}
	}
}

// A paired host must show what it is attached to and the URL to open, because
// that link is the reason the host is running at all.
func TestUnit_Host_PairedScreenShowsAttachmentAndAppLink(t *testing.T) {
	dir := t.TempDir()
	if err := relaycreds.Save(dir, relaycreds.Credentials{
		Endpoint:      "https://relay.contenox.com",
		InstanceToken: "secret-token-value",
		InstanceID:    "inst-42",
		AccountID:     "acct-7",
	}); err != nil {
		t.Fatalf("seed credentials: %v", err)
	}
	out := renderHostScreen(t, hostScreen{contenoxDir: dir, root: "/work", engineReady: true})

	for _, want := range []string{"attached to", "relay.contenox.com", "inst-42", "https://app.contenox.com"} {
		if !strings.Contains(out, want) {
			t.Fatalf("paired screen is missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "contenox pair <key>") {
		t.Fatalf("a paired host must not still be asking to be paired:\n%s", out)
	}
}

// Self-hosting is the same mechanism, not a second one: a relay on another
// domain, verified against its own public key, serves its own app. Such a host
// must be shown its own origin — never sent back to the hosted service it
// deliberately did not use.
func TestUnit_Host_SelfHostedRelayIsShownAsItself(t *testing.T) {
	dir := t.TempDir()
	if err := relaycreds.Save(dir, relaycreds.Credentials{
		Endpoint:       "https://relay.example.internal:8443",
		InstanceToken:  "tok",
		InstanceID:     "inst-9",
		RelayPublicKey: "AAAA",
	}); err != nil {
		t.Fatalf("seed credentials: %v", err)
	}
	out := renderHostScreen(t, hostScreen{contenoxDir: dir, root: "/work", engineReady: true})

	if !strings.Contains(out, "relay.example.internal:8443") {
		t.Fatalf("self-hosted relay not shown:\n%s", out)
	}
	if strings.Contains(out, "app.contenox.com") || strings.Contains(out, "relay.contenox.com") {
		t.Fatalf("a self-hosted host must not be pointed at the hosted service:\n%s", out)
	}
}

// The screen prints credentials-adjacent state, so the one field that is a
// secret must never reach it.
func TestUnit_Host_ScreenNeverPrintsTheInstanceToken(t *testing.T) {
	dir := t.TempDir()
	const token = "super-secret-instance-token"
	if err := relaycreds.Save(dir, relaycreds.Credentials{
		Endpoint:      "https://relay.contenox.com",
		InstanceToken: token,
		InstanceID:    "inst-42",
	}); err != nil {
		t.Fatalf("seed credentials: %v", err)
	}
	if out := renderHostScreen(t, hostScreen{contenoxDir: dir, root: "/work", engineReady: true}); strings.Contains(out, token) {
		t.Fatal("the instance token must never be printed")
	}
}

// A host with no model still serves, but saying "ready" would be a lie.
func TestUnit_Host_NoModelIsReportedNotHidden(t *testing.T) {
	out := renderHostScreen(t, hostScreen{contenoxDir: t.TempDir(), root: "/work", engineReady: false})
	if !strings.Contains(out, "no model configured") {
		t.Fatalf("screen must report a missing model:\n%s", out)
	}
	if !strings.Contains(out, "contenox setup") {
		t.Fatalf("screen must name the remedy:\n%s", out)
	}
}

// The log settings on screen have to come from the writer actually in use, not
// from restated defaults that could drift from what the operator configured.
func TestUnit_Host_ScreenReportsTheLogSettingsInForce(t *testing.T) {
	w, err := liblog.Open(liblog.Config{Dir: t.TempDir(), Name: "serve", MaxBytes: 2 << 20, MaxFiles: 3, MaxAge: 5 * 24 * time.Hour})
	if err != nil {
		t.Fatalf("open log: %v", err)
	}
	defer w.Close()

	out := renderHostScreen(t, hostScreen{contenoxDir: t.TempDir(), root: "/work", engineReady: true, log: w})
	if !strings.Contains(out, w.Path()) {
		t.Fatalf("screen must name the live log file:\n%s", out)
	}
	for _, want := range []string{"2MB", "3 files", "5 days"} {
		if !strings.Contains(out, want) {
			t.Fatalf("screen must state %q, the setting in force:\n%s", want, out)
		}
	}
}

// The screen names the key that changes what it just displayed, so an operator
// who does not like the bound does not have to go looking for its name.
func TestUnit_Host_ScreenNamesTheConfigKey(t *testing.T) {
	w, err := liblog.Open(liblog.Config{Dir: t.TempDir(), Name: "serve"})
	if err != nil {
		t.Fatalf("open log: %v", err)
	}
	defer w.Close()
	out := renderHostScreen(t, hostScreen{contenoxDir: t.TempDir(), root: "/w", engineReady: true, log: w})
	if !strings.Contains(out, "config set log-max-size") {
		t.Fatalf("screen must name the config key:\n%s", out)
	}
}

// "0 means no limit" is the operator's spelling in config; the screen must say
// so in words rather than printing a bare 0 that reads as "none kept".
func TestUnit_Host_UnlimitedRetentionReadsAsUnlimited(t *testing.T) {
	if got := keepDescription(liblog.Unlimited); got != "every file" {
		t.Fatalf("keepDescription(unlimited) = %q", got)
	}
	if got := ageDescription(liblog.Unlimited); got != "no age limit" {
		t.Fatalf("ageDescription(unlimited) = %q", got)
	}
	if got := ageDescription(24 * time.Hour); got != "1 day" {
		t.Fatalf("ageDescription(1d) = %q", got)
	}
}

// Colour is a terminal affordance; a redirected screen must stay plain so a
// captured status display is readable in a file or a bug report.
func TestUnit_Host_RedirectedScreenHasNoEscapes(t *testing.T) {
	if strings.Contains(renderHostScreen(t, hostScreen{contenoxDir: t.TempDir(), root: "/w"}), "\x1b") {
		t.Fatal("a non-terminal writer must get no ANSI escapes")
	}
}

// The host log must not depend on the telemetry toggle: a host that is not
// opted into telemetry still needs somewhere to write its own diagnostics.
func TestUnit_Host_LogIsSeparateFromTelemetry(t *testing.T) {
	if hostLogName == "telemetry" {
		t.Fatal("the host log must not share telemetry.log")
	}
	dir := t.TempDir()
	cmd := &cobra.Command{Use: "serve"}
	cmd.Flags().String("log-dir", dir, "")

	w, err := openHostLog(cmd)
	if err != nil {
		t.Fatalf("openHostLog: %v", err)
	}
	defer w.Close()
	if _, err := os.Stat(w.Path()); err != nil {
		t.Fatalf("log file was not created: %v", err)
	}
	if filepath.Base(w.Path()) == "telemetry.log" {
		t.Fatalf("host log collided with telemetry.log: %q", w.Path())
	}
	// Dated, so "what happened on Tuesday" is a question about filenames.
	if !strings.HasPrefix(filepath.Base(w.Path()), hostLogName+"-") {
		t.Fatalf("host log is not dated: %q", w.Path())
	}
}

// A host boots its log before the database exists, then adopts the stored
// bounds. This pins that second step, which is the only reason config keys
// can bound a file that was already open.
func TestUnit_Host_StoredSettingsReachTheLiveLog(t *testing.T) {
	dir := t.TempDir()
	cmd := &cobra.Command{Use: "serve"}
	cmd.Flags().String("log-dir", dir, "")

	w, err := openHostLog(cmd)
	if err != nil {
		t.Fatalf("openHostLog: %v", err)
	}
	defer w.Close()
	if w.MaxBytes() != liblog.DefaultMaxBytes {
		t.Fatalf("expected the boot default, got %d", w.MaxBytes())
	}

	// What logSettingsFromConfig would have produced for these stored values.
	w.Reconfigure(50<<20, 4, 7*24*time.Hour)
	if w.MaxBytes() != 50<<20 || w.MaxFiles() != 4 || w.MaxAge() != 7*24*time.Hour {
		t.Fatalf("stored settings did not reach the live log: %d/%d/%s", w.MaxBytes(), w.MaxFiles(), w.MaxAge())
	}
}

// `config set` is where a bad bound must be refused: at boot the only options
// are to ignore it or refuse to start, and by then nobody is watching.
func TestUnit_Host_LogConfigIsValidatedAtSetTime(t *testing.T) {
	for _, tc := range []struct{ key, value string }{
		{"log-max-size", "enormous"},
		{"log-max-size", "-1MB"},
		{"log-max-files", "many"},
		{"log-max-files", "-2"},
		{"log-max-age-days", "3.5"},
	} {
		if _, err := normalizeLogConfig(tc.key, tc.value); err == nil {
			t.Fatalf("normalizeLogConfig(%q, %q) = nil error, want a refusal", tc.key, tc.value)
		}
	}
}

// A size is stored canonically so `config get` reads back what the log uses.
func TestUnit_Host_LogSizeIsStoredCanonically(t *testing.T) {
	got, err := normalizeLogConfig("log-max-size", "  50 mb ")
	if err != nil {
		t.Fatalf("normalizeLogConfig: %v", err)
	}
	if got != "50MB" {
		t.Fatalf("stored %q, want the canonical %q", got, "50MB")
	}
}

// Keys this validator does not own must pass through untouched, or adding a
// log key would start rewriting unrelated config values.
func TestUnit_Host_LogValidatorIgnoresOtherKeys(t *testing.T) {
	got, err := normalizeLogConfig("default-model", "qwen3:8b")
	if err != nil {
		t.Fatalf("unexpected error for an unrelated key: %v", err)
	}
	if got != "" {
		t.Fatalf("validator rewrote an unrelated key to %q", got)
	}
}

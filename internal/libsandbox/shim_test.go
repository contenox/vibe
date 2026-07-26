//go:build linux

package libsandbox_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/contenox/beam/internal/libsandbox"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

// The shim + probe use the standard os/exec re-exec-the-test-binary pattern, but
// two layers deep. Command re-execs this test binary as the sandbox shim
// (layer 1); ShimMain applies Landlock and execve's the test binary again as a
// probe (layer 2); the probe performs ONE filesystem operation and reports the
// outcome as its exit code. TestMain dispatches by which layer we are.
const (
	probeEnv     = "CONTENOX_SANDBOX_PROBE_ACTION" // "read"|"write"|"net-enum"|"net-connect"|"noop"
	probePathEnv = "CONTENOX_SANDBOX_PROBE_PATH"

	exitAllowed  = 0 // the operation succeeded — the wall permits it
	exitDenied   = 3 // EACCES/EPERM — the wall blocked it
	exitOther    = 4 // some other error (test bug / unexpected)
	exitShimFail = 5 // the shim itself failed before it could exec the probe

	// Network-probe outcomes (see probeNetEnum / probeNetConnect).
	exitNetLoOnlyUp    = 6  // enumerate: only loopback present, and it is UP (expected)
	exitNetExternal    = 7  // enumerate: a non-loopback interface exists (wall breached)
	exitNetLoDown      = 8  // enumerate: loopback present but DOWN
	exitNetNoIface     = 9  // enumerate: no interfaces at all
	exitNetUnreachable = 10 // connect: outbound refused, network-unreachable (deny holds)
	exitNetConnected   = 11 // connect: outbound SUCCEEDED (wall breached)
	exitNetConnOther   = 12 // connect: failed with some other error
)

func TestMain(m *testing.M) {
	// Layer 1: re-exec'd by libsandbox.Command as the sandbox shim. On success
	// ShimMain execve's the probe and never returns; a return means it failed.
	if handled, err := libsandbox.ShimMain(); handled {
		fmt.Fprintln(os.Stderr, "sandbox shim:", err)
		os.Exit(exitShimFail)
	}
	// A support probe: a bare clone (no shim) that checks TUN creation in the netns.
	if os.Getenv(egressTunCheckEnv) != "" {
		egressTunCheckChild()
		return
	}
	// Layer 2: execve'd by the shim, now confined. Run the one probe and exit.
	if action := os.Getenv(probeEnv); action != "" {
		os.Exit(runProbe(action, os.Getenv(probePathEnv)))
	}
	// Ordinary test run.
	os.Exit(m.Run())
}

// runProbe performs a single filesystem operation under the wall and maps the
// result to an exit code: success = allowed, a permission error = denied.
func runProbe(action, path string) int {
	switch action {
	case "read":
		f, err := os.Open(path)
		if err != nil {
			return classify(err)
		}
		defer f.Close()
		if _, err := f.Read(make([]byte, 1)); err != nil && !errors.Is(err, io.EOF) {
			return classify(err)
		}
		return exitAllowed
	case "write":
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE, 0o600)
		if err != nil {
			return classify(err)
		}
		defer f.Close()
		if _, err := f.Write([]byte("x")); err != nil {
			return classify(err)
		}
		return exitAllowed
	case "net-enum":
		return probeNetEnum()
	case "net-connect":
		return probeNetConnect()
	case "egress-allow", "egress-dns-deny", "egress-connect-deny", "egress-guarded-connect":
		return runEgressProbe(action)
	case "tap-exec":
		return runTapExecProbe()
	case "noop":
		// Used only by usernsNetnsSupported: the child just needs to start under
		// the clone flags and exit cleanly to prove the namespaces are creatable.
		return exitAllowed
	default:
		return exitOther
	}
}

// probeNetEnum enumerates the interfaces visible inside the wall. Deny-by-
// construction means the fresh netns has no external interface; the loopback
// decision means "lo" is brought up. So the one healthy outcome is exactly
// loopback, UP, and nothing else — anything non-loopback is a breach.
func probeNetEnum() int {
	ifaces, err := net.Interfaces()
	if err != nil {
		return exitOther
	}
	sawLo, loUp := false, false
	for _, ifi := range ifaces {
		if ifi.Flags&net.FlagLoopback != 0 {
			sawLo = true
			loUp = loUp || ifi.Flags&net.FlagUp != 0
			continue
		}
		return exitNetExternal // any non-loopback interface means the wall leaked
	}
	switch {
	case !sawLo:
		return exitNetNoIface
	case loUp:
		return exitNetLoOnlyUp
	default:
		return exitNetLoDown
	}
}

// probeNetConnect attempts an outbound TCP connection to a routable literal
// address (an IP literal, so there is no DNS lookup — there is no resolver in
// here anyway). In the empty netns with no default route the kernel refuses it
// immediately with ENETUNREACH, proving the network is absent by construction; a
// success would mean the wall leaked. 192.0.2.1 is TEST-NET-1 (RFC 5737),
// reserved and never a real destination.
func probeNetConnect() int {
	c, err := net.DialTimeout("tcp", "192.0.2.1:80", 3*time.Second)
	if err == nil {
		_ = c.Close()
		return exitNetConnected
	}
	if errors.Is(err, unix.ENETUNREACH) || errors.Is(err, unix.EHOSTUNREACH) {
		return exitNetUnreachable
	}
	return exitNetConnOther
}

func classify(err error) int {
	if errors.Is(err, os.ErrPermission) {
		return exitDenied
	}
	return exitOther
}

// landlockSupported reports whether the running kernel exposes a usable Landlock
// filesystem ABI, so the wall tests can skip gracefully where it is absent.
func landlockSupported() bool {
	r, _, e := unix.Syscall(unix.SYS_LANDLOCK_CREATE_RULESET, 0, 0, uintptr(unix.LANDLOCK_CREATE_RULESET_VERSION))
	return e == 0 && int(r) >= 1
}

// usernsNetnsSupported reports whether this host lets an unprivileged process
// create a user+network namespace — the precondition for the network wall (and,
// now, for the fs wall too, since Command clones into both). It does the
// definitive check: actually attempt the exact clone the sandbox uses, re-exec'ing
// this test binary with the "noop" probe (which exits 0 the moment it starts). A
// kernel or an AppArmor/sysctl policy that forbids the clone makes cmd.Run return
// non-nil. This is more reliable than reading the sysctls, which do not capture an
// AppArmor restriction (e.g. Ubuntu 24.04). The wall tests skip when it is false.
func usernsNetnsSupported() bool {
	cmd := exec.Command("/proc/self/exe")
	cmd.Env = append(os.Environ(), probeEnv+"=noop")
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Cloneflags:                 unix.CLONE_NEWUSER | unix.CLONE_NEWNET,
		UidMappings:                []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getuid(), Size: 1}},
		GidMappings:                []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getgid(), Size: 1}},
		GidMappingsEnableSetgroups: false,
	}
	return cmd.Run() == nil
}

func exitCode(err error) int {
	if err == nil {
		return 0
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode()
	}
	return -1
}

// TestIntegration_FSWall drives the whole seam end to end: for each probe it
// asks libsandbox.Command to confine THIS test binary (re-exec'd as the shim,
// then the probe), runs it, and asserts allowed/denied from the probe's exit
// code. It proves the wall lets the workspace and a ro carve-out through, blocks
// a write to the ro hole, and blocks reads/writes outside the wall — including a
// "~/.contenox"-shaped loot path under a pretend real home.
//
// As of slice 3 Command also clones into a fresh user+network namespace, so this
// suite doubles as the composition regression: the fs assertions must still hold
// with the namespaces in place (Landlock jails the unchanged host mounts inside
// the new namespaces, and the system-runtime read grant still lets the
// dynamically linked probe binary load).
func TestIntegration_FSWall(t *testing.T) {
	if !landlockSupported() {
		t.Skip("landlock filesystem ABI unavailable on this kernel")
	}
	if !usernsNetnsSupported() {
		t.Skip("unprivileged user+network namespaces unavailable on this host")
	}

	ws := t.TempDir()       // the one writable root
	home := t.TempDir()     // scoped HOME + the "~" resolution anchor
	realHome := t.TempDir() // pretend operator home — NOT under the wall
	secret := t.TempDir()   // a loot directory outside the wall

	// A file inside the workspace.
	wsFile := filepath.Join(ws, "notes.txt")
	require.NoError(t, os.WriteFile(wsFile, []byte("work"), 0o600))

	// A ro carve-out, reached as "~/.claude" → <scoped home>/.claude.
	claudeDir := filepath.Join(home, ".claude")
	require.NoError(t, os.MkdirAll(claudeDir, 0o700))
	claudeFile := filepath.Join(claudeDir, "config.json")
	require.NoError(t, os.WriteFile(claudeFile, []byte("auth"), 0o600))

	// A secret outside the wall (an ~/.ssh-shaped loot file).
	secretFile := filepath.Join(secret, "id_rsa")
	require.NoError(t, os.WriteFile(secretFile, []byte("PRIVATE KEY"), 0o600))

	// A "~/.contenox"-shaped control-plane path under the pretend real home. The
	// scoped HOME means the agent's "~" is elsewhere, so this must be unreachable.
	contenoxDir := filepath.Join(realHome, ".contenox")
	require.NoError(t, os.MkdirAll(contenoxDir, 0o700))
	contenoxFile := filepath.Join(contenoxDir, "state.json")
	require.NoError(t, os.WriteFile(contenoxFile, []byte("cfg"), 0o600))

	newSpec := func() libsandbox.Spec {
		return libsandbox.Spec{
			WorkspaceRoot: ws,
			Home:          home,
			FS: []libsandbox.FSCarveout{
				{Path: "~/.claude", Mode: libsandbox.ModeRO, Needs: "agent auth/config to start"},
			},
		}
	}

	cases := []struct {
		name   string
		action string
		path   string
		want   int
	}{
		{"read inside workspace is allowed", "read", wsFile, exitAllowed},
		{"read a ro carve-out is allowed", "read", claudeFile, exitAllowed},
		{"write to a ro carve-out is denied", "write", claudeFile, exitDenied},
		{"read a secret outside the wall is denied", "read", secretFile, exitDenied},
		{"read a ~/.contenox-shaped path is denied", "read", contenoxFile, exitDenied},
		{"write outside the workspace is denied", "write", filepath.Join(secret, "planted"), exitDenied},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			spec := newSpec()
			// The probe action/path travel to layer 2 as scrubbed env (EnvSet
			// survives the scrub; the shim strips only its own transport vars).
			spec.EnvSet = map[string]string{
				probeEnv:     tc.action,
				probePathEnv: tc.path,
			}

			cmd, err := libsandbox.Command(context.Background(), spec, "/proc/self/exe")
			require.NoError(t, err)

			runErr := cmd.Run()
			require.Equal(t, tc.want, exitCode(runErr),
				"probe %q on %q: unexpected outcome (runErr=%v)", tc.action, tc.path, runErr)
		})
	}
}

// TestIntegration_TildeResolvesAgainstScopedHome pins the "~" behaviour: a
// "~/.claude" carve-out reaches the file under the SCOPED home, while the same
// relative name under a different (real) home is not reachable — the loot paths
// stay out precisely because "~" resolves into the scoped home, not the real one.
func TestIntegration_TildeResolvesAgainstScopedHome(t *testing.T) {
	if !landlockSupported() {
		t.Skip("landlock filesystem ABI unavailable on this kernel")
	}
	if !usernsNetnsSupported() {
		t.Skip("unprivileged user+network namespaces unavailable on this host")
	}

	ws := t.TempDir()
	home := t.TempDir()

	scopedClaude := filepath.Join(home, ".claude", "config.json")
	require.NoError(t, os.MkdirAll(filepath.Dir(scopedClaude), 0o700))
	require.NoError(t, os.WriteFile(scopedClaude, []byte("auth"), 0o600))

	spec := libsandbox.Spec{
		WorkspaceRoot: ws,
		Home:          home,
		FS: []libsandbox.FSCarveout{
			{Path: "~/.claude", Mode: libsandbox.ModeRO, Needs: "agent auth"},
		},
		EnvSet: map[string]string{probeEnv: "read", probePathEnv: scopedClaude},
	}

	cmd, err := libsandbox.Command(context.Background(), spec, "/proc/self/exe")
	require.NoError(t, err)
	require.Equal(t, exitAllowed, exitCode(cmd.Run()),
		"~/.claude should resolve into the scoped home and be readable")
}

// TestIntegration_NetWall drives the deny-by-construction network FLOOR end to
// end. It confines THIS test binary through the full seam (Landlock + the
// user/network namespaces) with NO network carve-outs and asserts, from inside
// the wall, that the network is absent by construction: enumerating interfaces
// shows only loopback (brought up, nothing external), and an outbound connection
// to a routable address is refused as network-unreachable. Together they prove
// the fresh netns has no route out while localhost still works.
//
// This is the slice-3 net-isolation regression under slice 4: with no carve-outs
// the egress bridge is never built, so the floor is exactly as before — no TUN,
// no route. (The metered-egress path a carve-out opens is exercised separately by
// TestIntegration_NetEgress; a carve-out is deliberately NOT included here, since
// as of slice 4 it would legitimately create a route and this floor asserts its
// absence.)
func TestIntegration_NetWall(t *testing.T) {
	if !landlockSupported() {
		t.Skip("landlock filesystem ABI unavailable on this kernel")
	}
	if !usernsNetnsSupported() {
		t.Skip("unprivileged user+network namespaces unavailable on this host")
	}

	ws := t.TempDir()
	home := t.TempDir()

	newSpec := func(action string) libsandbox.Spec {
		return libsandbox.Spec{
			WorkspaceRoot: ws,
			Home:          home,
			NetworkWall:   true, // this is the network-FLOOR test; opt into the wall it asserts
			EnvSet:        map[string]string{probeEnv: action},
		}
	}

	cases := []struct {
		name   string
		action string
		want   int
	}{
		{"only loopback is visible, and it is up", "net-enum", exitNetLoOnlyUp},
		{"outbound connect is network-unreachable", "net-connect", exitNetUnreachable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cmd, err := libsandbox.Command(context.Background(), newSpec(tc.action), "/proc/self/exe")
			require.NoError(t, err)
			require.Equal(t, tc.want, exitCode(cmd.Run()),
				"net probe %q: unexpected outcome", tc.action)
		})
	}
}

// TestUnit_Command_RejectsRelativeWorkspaceOnLinux checks the Linux seam's
// absoluteness precondition: a relative workspace cannot anchor a cwd or a
// Landlock rule, so it is rejected as an invalid spec.
func TestUnit_Command_RejectsRelativeWorkspaceOnLinux(t *testing.T) {
	_, err := libsandbox.Command(context.Background(), libsandbox.Spec{
		WorkspaceRoot: "relative/ws",
		Home:          "/h",
	}, "/bin/true")
	require.ErrorIs(t, err, libsandbox.ErrInvalidSpec)
}

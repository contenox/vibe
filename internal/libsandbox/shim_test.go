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

	"github.com/contenox/contenox/internal/libsandbox"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

const (
	probeEnv     = "CONTENOX_SANDBOX_PROBE_ACTION"
	probePathEnv = "CONTENOX_SANDBOX_PROBE_PATH"

	exitAllowed  = 0
	exitDenied   = 3
	exitOther    = 4
	exitShimFail = 5

	exitNetLoOnlyUp    = 6
	exitNetExternal    = 7
	exitNetLoDown      = 8
	exitNetNoIface     = 9
	exitNetUnreachable = 10
	exitNetConnected   = 11
	exitNetConnOther   = 12
)

func TestMain(m *testing.M) {
	// Layer 1: on success ShimMain execve's the probe and never returns.
	if handled, err := libsandbox.ShimMain(); handled {
		fmt.Fprintln(os.Stderr, "sandbox shim:", err)
		os.Exit(exitShimFail)
	}
	// Support probe: a bare clone (no shim) checking TUN creation in the netns.
	if os.Getenv(egressTunCheckEnv) != "" {
		egressTunCheckChild()
		return
	}
	// Layer 2: confined. Run the one probe and exit.
	if action := os.Getenv(probeEnv); action != "" {
		os.Exit(runProbe(action, os.Getenv(probePathEnv)))
	}
	// Ordinary test run.
	os.Exit(m.Run())
}

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
		return exitAllowed // proves the namespaces are creatable (usernsNetnsSupported)
	default:
		return exitOther
	}
}

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

func landlockSupported() bool {
	r, _, e := unix.Syscall(unix.SYS_LANDLOCK_CREATE_RULESET, 0, 0, uintptr(unix.LANDLOCK_CREATE_RULESET_VERSION))
	return e == 0 && int(r) >= 1
}

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

// TestIntegration_FSWall drives the whole seam end to end via Command: the wall lets the workspace and a ro carve-out through, blocks a write to the ro hole, and blocks reads/writes outside the wall (including with the user+network namespaces in place).
func TestIntegration_FSWall(t *testing.T) {
	if !landlockSupported() {
		t.Skip("landlock filesystem ABI unavailable on this kernel")
	}
	if !usernsNetnsSupported() {
		t.Skip("unprivileged user+network namespaces unavailable on this host")
	}

	ws := t.TempDir()
	home := t.TempDir()
	realHome := t.TempDir() // NOT under the wall
	secret := t.TempDir()   // outside the wall

	wsFile := filepath.Join(ws, "notes.txt")
	require.NoError(t, os.WriteFile(wsFile, []byte("work"), 0o600))

	// A ro carve-out, reached as "~/.claude" → <scoped home>/.claude.
	claudeDir := filepath.Join(home, ".claude")
	require.NoError(t, os.MkdirAll(claudeDir, 0o700))
	claudeFile := filepath.Join(claudeDir, "config.json")
	require.NoError(t, os.WriteFile(claudeFile, []byte("auth"), 0o600))

	// Outside the wall (an ~/.ssh-shaped loot file).
	secretFile := filepath.Join(secret, "id_rsa")
	require.NoError(t, os.WriteFile(secretFile, []byte("PRIVATE KEY"), 0o600))

	// A "~/.contenox"-shaped path under the pretend real home; the scoped HOME means the agent's "~" is elsewhere, so this must be unreachable.
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

// TestIntegration_TildeResolvesAgainstScopedHome pins that a "~/.claude"
// carve-out resolves against the scoped home, not a different real home.
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

// TestIntegration_NetWall pins the deny-by-construction network floor with no carve-outs: only loopback is visible and an outbound connection is refused as network-unreachable.
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
			NetworkWall:   true,
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

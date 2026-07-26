//go:build linux

package libsandbox_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"unsafe"

	"github.com/contenox/beam/internal/libsandbox"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

// seccompUserNotifSupported mirrors the library's own capability probe so the tap
// tests skip gracefully on a kernel without SECCOMP_RET_USER_NOTIF (needs
// SECCOMP_FILTER_FLAG_NEW_LISTENER, ~5.0+). It is unprivileged and installs no
// filter.
func seccompUserNotifSupported() bool {
	action := uint32(unix.SECCOMP_RET_USER_NOTIF)
	_, _, e := unix.Syscall(unix.SYS_SECCOMP,
		uintptr(unix.SECCOMP_GET_ACTION_AVAIL), 0, uintptr(unsafe.Pointer(&action)))
	return e == 0
}

// runTapExecProbe (layer 2, the confined agent) execs the path in probePathEnv. On
// a successful exec the image is replaced, so a return means exec failed; classify
// maps a permission error (Landlock deny) to exitDenied. It is the syscall the tap
// observes.
func runTapExecProbe() int {
	path := os.Getenv(probePathEnv)
	if path == "" {
		return exitOther
	}
	err := syscall.Exec(path, []string{path}, os.Environ())
	return classify(err)
}

// kvString extracts a string value for key from a flat kvArgs slice.
func kvString(kv []any, key string) string {
	for i := 0; i+1 < len(kv); i += 2 {
		if k, ok := kv[i].(string); ok && k == key {
			if v, ok := kv[i+1].(string); ok {
				return v
			}
		}
	}
	return ""
}

// hasSyscall reports whether a "sandbox-syscall" event was recorded for the given
// syscall whose reported path contains pathSubstr (pass "" to match any path).
func (t *recTracker) hasSyscall(syscallName, pathSubstr string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, e := range t.events {
		if e.subj != "sandbox-syscall" || !e.allow {
			continue
		}
		if kvString(e.kv, "syscall") != syscallName {
			continue
		}
		if pathSubstr == "" || strings.Contains(e.id, pathSubstr) {
			return true
		}
	}
	return false
}

// findAllowedExecutable returns a stock executable Landlock permits (it lives under
// systemRuntimePaths) that exits 0, or skips if the host has none.
func findAllowedExecutable(t *testing.T) string {
	t.Helper()
	for _, p := range []string{"/bin/true", "/usr/bin/true", "/bin/echo", "/usr/bin/echo"} {
		if fi, err := os.Stat(p); err == nil && fi.Mode().IsRegular() {
			return p
		}
	}
	t.Skip("no stock executable (/bin/true, /bin/echo, …) present to exec")
	return ""
}

func requireTapPreconditions(t *testing.T) {
	t.Helper()
	if !landlockSupported() {
		t.Skip("landlock filesystem ABI unavailable on this kernel")
	}
	if !usernsNetnsSupported() {
		t.Skip("unprivileged user+network namespaces unavailable on this host")
	}
	if !seccompUserNotifSupported() {
		t.Skip("seccomp user-notify (SECCOMP_RET_USER_NOTIF, kernel 5.0+) unavailable on this kernel")
	}
}

// TestIntegration_SyscallTap_RecordsAndAllowsExec proves record-then-ALLOW on the
// happy path: with the tap on, the confined agent execs a stock binary; the tap
// records the attempt as a "sandbox-syscall" event naming the exec'd path AND the
// exec still SUCCEEDS (the binary actually runs, exit 0) — the tap observed without
// gating.
func TestIntegration_SyscallTap_RecordsAndAllowsExec(t *testing.T) {
	requireTapPreconditions(t)
	execTarget := findAllowedExecutable(t)

	ws := t.TempDir()
	home := t.TempDir()
	tracker := &recTracker{}

	spec := libsandbox.Spec{
		WorkspaceRoot: ws,
		Home:          home,
		SyscallTap:    true,
		Tracker:       tracker,
		EnvSet: map[string]string{
			probeEnv:     "tap-exec",
			probePathEnv: execTarget,
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cmd, err := libsandbox.Command(ctx, spec, "/proc/self/exe")
	require.NoError(t, err)
	require.Equal(t, exitAllowed, exitCode(cmd.Run()),
		"record-then-allow: the tapped exec of %q must still SUCCEED (the probe actually ran)", execTarget)

	require.True(t, tracker.hasSyscall("execve", execTarget),
		"the tap must record a %q sandbox-syscall event naming the exec'd path %q", "execve", execTarget)
}

// TestIntegration_SyscallTap_RecordsDeniedExec proves the tap sees bypass attempts
// the floor would swallow: the agent execs a path OUTSIDE the wall (not the
// workspace, a carve-out, or a system path). Landlock denies it (EACCES), yet the
// tap STILL records the attempt — the syscall-entry notification fires before the
// kernel's Landlock check, so an attempt that is denied there is observed here.
func TestIntegration_SyscallTap_RecordsDeniedExec(t *testing.T) {
	requireTapPreconditions(t)

	ws := t.TempDir()
	home := t.TempDir()
	secret := t.TempDir() // outside the wall, not carved out

	deniedProg := filepath.Join(secret, "prog")
	// Deliberately NOT a valid executable: the ONLY way this returns "denied" is a
	// Landlock EACCES at exec time. If the wall ever leaked, the exec would instead
	// fail ENOEXEC → exitOther and this test would fail loudly.
	require.NoError(t, os.WriteFile(deniedProg, []byte("x"), 0o755))

	tracker := &recTracker{}
	spec := libsandbox.Spec{
		WorkspaceRoot: ws,
		Home:          home,
		SyscallTap:    true,
		Tracker:       tracker,
		EnvSet: map[string]string{
			probeEnv:     "tap-exec",
			probePathEnv: deniedProg,
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cmd, err := libsandbox.Command(ctx, spec, "/proc/self/exe")
	require.NoError(t, err)
	require.Equal(t, exitDenied, exitCode(cmd.Run()),
		"the exec of a path outside the wall must be DENIED by Landlock")

	require.True(t, tracker.hasSyscall("execve", deniedProg),
		"the tap must STILL record the DENIED execve attempt naming %q", deniedProg)
}

// TestIntegration_SyscallTap_WallUnchanged is the regression that the tap is
// telemetry, not enforcement: with SyscallTap ON, the fs-wall verdicts are exactly
// as with it off (the existing suites cover the off case) — an allowed read still
// succeeds, a read/write outside the wall is still denied. The tap adds observation
// without changing what the wall permits.
func TestIntegration_SyscallTap_WallUnchanged(t *testing.T) {
	requireTapPreconditions(t)

	ws := t.TempDir()
	home := t.TempDir()
	secret := t.TempDir()

	wsFile := filepath.Join(ws, "notes.txt")
	require.NoError(t, os.WriteFile(wsFile, []byte("work"), 0o600))
	secretFile := filepath.Join(secret, "id_rsa")
	require.NoError(t, os.WriteFile(secretFile, []byte("PRIVATE KEY"), 0o600))

	cases := []struct {
		name   string
		action string
		path   string
		want   int
	}{
		{"read inside workspace still allowed", "read", wsFile, exitAllowed},
		{"read a secret outside the wall still denied", "read", secretFile, exitDenied},
		{"write outside the workspace still denied", "write", filepath.Join(secret, "planted"), exitDenied},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			spec := libsandbox.Spec{
				WorkspaceRoot: ws,
				Home:          home,
				SyscallTap:    true,
				Tracker:       &recTracker{},
				EnvSet:        map[string]string{probeEnv: tc.action, probePathEnv: tc.path},
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			cmd, err := libsandbox.Command(ctx, spec, "/proc/self/exe")
			require.NoError(t, err)
			require.Equal(t, tc.want, exitCode(cmd.Run()),
				"with the tap ON, the wall verdict for %q on %q must be unchanged", tc.action, tc.path)
		})
	}
}

// TestUnit_SyscallTap_UnsupportedKernelFailsClosed asserts the opt-in tap fails
// closed up front on a kernel without user-notify: Command returns ErrIsolation
// rather than silently running the agent unobserved. It only runs where the tap is
// genuinely unavailable (otherwise there is nothing to assert).
func TestUnit_SyscallTap_UnsupportedKernelFailsClosed(t *testing.T) {
	if seccompUserNotifSupported() {
		t.Skip("seccomp user-notify IS supported here; nothing to assert about the unsupported path")
	}
	_, err := libsandbox.Command(context.Background(), libsandbox.Spec{
		WorkspaceRoot: "/tmp",
		Home:          "/tmp",
		SyscallTap:    true,
	}, "/bin/true")
	require.ErrorIs(t, err, libsandbox.ErrIsolation)
}

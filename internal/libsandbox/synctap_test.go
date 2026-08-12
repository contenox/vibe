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

	"github.com/contenox/contenox/internal/libsandbox"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

func seccompUserNotifSupported() bool {
	action := uint32(unix.SECCOMP_RET_USER_NOTIF)
	_, _, e := unix.Syscall(unix.SYS_SECCOMP,
		uintptr(unix.SECCOMP_GET_ACTION_AVAIL), 0, uintptr(unsafe.Pointer(&action)))
	return e == 0
}

func runTapExecProbe() int {
	path := os.Getenv(probePathEnv)
	if path == "" {
		return exitOther
	}
	err := syscall.Exec(path, []string{path}, os.Environ())
	return classify(err)
}

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

// TestIntegration_SyscallTap_RecordsAndAllowsExec pins record-then-allow: with
// the tap on, an exec of a stock binary is recorded as a "sandbox-syscall"
// event naming the path, and still succeeds.
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

// TestIntegration_SyscallTap_RecordsDeniedExec pins that the tap still records
// an exec Landlock denies (EACCES): the notification fires before the
// kernel's Landlock check, so a denied attempt is still observed.
func TestIntegration_SyscallTap_RecordsDeniedExec(t *testing.T) {
	requireTapPreconditions(t)

	ws := t.TempDir()
	home := t.TempDir()
	secret := t.TempDir() // outside the wall, not carved out

	deniedProg := filepath.Join(secret, "prog")
	// Deliberately not a valid executable, so a leaked wall would fail ENOEXEC (exitOther) instead of the expected Landlock EACCES.
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

// TestIntegration_SyscallTap_WallUnchanged pins that the tap is telemetry, not
// enforcement: fs-wall verdicts are unchanged with SyscallTap on.
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

// TestUnit_SyscallTap_UnsupportedKernelFailsClosed pins that the opt-in tap
// fails closed on a kernel without user-notify (Command returns ErrIsolation).
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

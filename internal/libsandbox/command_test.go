package libsandbox_test

import (
	"context"
	"runtime"
	"testing"

	"github.com/contenox/beam/internal/libsandbox"
	"github.com/contenox/beam/internal/libtracker"
	"github.com/stretchr/testify/require"
)

// fakeTracker counts ActivityTracker lifecycle calls so a test can assert the
// wall is instrumented from the start.
type fakeTracker struct {
	starts, ends, changes int
	errs                  []error
}

func (f *fakeTracker) Start(ctx context.Context, operation, subject string, kvArgs ...any) (func(error), func(string, any), func()) {
	f.starts++
	return func(err error) { f.errs = append(f.errs, err) },
		func(string, any) { f.changes++ },
		func() { f.ends++ }
}

var _ libtracker.ActivityTracker = (*fakeTracker)(nil)

// A minimal valid spec assembles a command with the environment scrubbed, HOME
// forced to the scoped home, and the working directory pinned to the workspace.
// Gated to Linux: off Linux the wall cannot be built, so Command fails closed
// (see TestUnit_Command_FailsClosedOffLinux) and returns no command to inspect.
func TestUnit_Command_AssemblesMinimalValidSpec(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Command assembles a runnable command only on Linux; off Linux it fails closed")
	}
	ws := t.TempDir()
	home := t.TempDir()
	t.Setenv("PATH", "/usr/bin")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "shhh")

	cmd, err := libsandbox.Command(context.Background(), libsandbox.Spec{
		WorkspaceRoot: ws,
		Home:          home,
		EnvAllow:      []string{"PATH"},
	}, "true", "arg1")

	require.NoError(t, err)
	require.NotNil(t, cmd)
	require.Equal(t, ws, cmd.Dir)
	require.Equal(t, []string{"true", "arg1"}, cmd.Args)
	// PATH is the operator's PATH filtered to the exec surface: /usr/bin is within
	// SystemExecDirs, so it survives; a secret var never rides along.
	require.Contains(t, cmd.Env, "PATH=/usr/bin")
	require.Contains(t, cmd.Env, "HOME="+home)
	for _, kv := range cmd.Env {
		require.NotContains(t, kv, "AWS_SECRET_ACCESS_KEY")
	}
}

// The regression scenario, fixed: the confined PATH is the operator's PATH filtered
// to the exec surface. A toolchain dir UNDER a carve-out (node under a carved
// ~/.nvm-style tree) survives so the agent can find node; an UNcarved profile dir is
// dropped. This is what makes a `#!/usr/bin/env node` agent resolve its interpreter
// under confinement. Gated to Linux, where Command assembles a runnable command.
func TestUnit_Command_ConfinedPathKeepsCarvedToolchainDropsUncarved(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Command assembles a runnable command only on Linux")
	}
	// Operator PATH: a carved toolchain bin, a system dir, and an UNcarved profile dir.
	t.Setenv("PATH", "/opt/toolchain/node/bin:/usr/bin:/home/dev/.cargo/bin")

	cmd, err := libsandbox.Command(context.Background(), libsandbox.Spec{
		WorkspaceRoot: t.TempDir(),
		Home:          t.TempDir(),
		FS:            []libsandbox.FSCarveout{{Path: "/opt/toolchain", Mode: "ro", Needs: "node runtime the agent execs"}},
	}, "true")

	require.NoError(t, err)
	// The carved toolchain bin and the system dir survive; the uncarved cargo dir does not.
	require.Contains(t, cmd.Env, "PATH=/opt/toolchain/node/bin:/usr/bin")
}

// An EnvSet PATH override that names a directory with no matching FS carve-out is
// hard-rejected before the wall is built — fail-closed, with the offending entry
// surfaced as ErrInvalidSpec rather than a run-time Landlock EACCES. This runs on
// every platform because the rejection precedes applyIsolation.
func TestUnit_Command_RejectsPathOutsideExecSurface(t *testing.T) {
	_, err := libsandbox.Command(context.Background(), libsandbox.Spec{
		WorkspaceRoot: "/ws",
		Home:          "/h",
		EnvSet:        map[string]string{"PATH": "/opt/rogue/bin"},
	}, "true")

	require.ErrorIs(t, err, libsandbox.ErrInvalidSpec)
}

// A relative EnvSet PATH entry — the implicit-current-directory exec hazard — is
// rejected for the same reason, on every platform.
func TestUnit_Command_RejectsRelativePathEntry(t *testing.T) {
	_, err := libsandbox.Command(context.Background(), libsandbox.Spec{
		WorkspaceRoot: "/ws",
		Home:          "/h",
		EnvSet:        map[string]string{"PATH": "/usr/bin:relative/bin"},
	}, "true")

	require.ErrorIs(t, err, libsandbox.ErrInvalidSpec)
}

// An EnvSet PATH override IS admitted when the directory is covered by a declared
// FS carve-out: the coupling the design intends — a toolchain dir on PATH only if
// it is also granted through the wall. Gated to Linux, where a valid spec
// assembles a real command (off Linux Command fails closed regardless).
func TestUnit_Command_AllowsPathWithinCarveout(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("a successful Command assembly is only reachable on Linux")
	}
	cmd, err := libsandbox.Command(context.Background(), libsandbox.Spec{
		WorkspaceRoot: t.TempDir(),
		Home:          t.TempDir(),
		EnvSet:        map[string]string{"PATH": "/opt/tools/bin"},
		FS:            []libsandbox.FSCarveout{{Path: "/opt/tools", Mode: "ro", Needs: "toolchain on PATH"}},
	}, "true")

	require.NoError(t, err)
	require.Contains(t, cmd.Env, "PATH=/opt/tools/bin")
}

func TestUnit_Command_RejectsEmptyWorkspace(t *testing.T) {
	_, err := libsandbox.Command(context.Background(),
		libsandbox.Spec{Home: "/h"}, "true")

	require.ErrorIs(t, err, libsandbox.ErrInvalidSpec)
}

func TestUnit_Command_RejectsEmptyHome(t *testing.T) {
	_, err := libsandbox.Command(context.Background(),
		libsandbox.Spec{WorkspaceRoot: "/ws"}, "true")

	require.ErrorIs(t, err, libsandbox.ErrInvalidSpec)
}

func TestUnit_Command_RejectsEmptyName(t *testing.T) {
	_, err := libsandbox.Command(context.Background(),
		libsandbox.Spec{WorkspaceRoot: "/ws", Home: "/h"}, "")

	require.ErrorIs(t, err, libsandbox.ErrInvalidSpec)
}

func TestUnit_Command_RejectsInvalidCarveout(t *testing.T) {
	_, err := libsandbox.Command(context.Background(), libsandbox.Spec{
		WorkspaceRoot: "/ws",
		Home:          "/h",
		FS:            []libsandbox.FSCarveout{{Path: "../etc", Mode: "ro", Needs: "x"}},
	}, "true")

	require.ErrorIs(t, err, libsandbox.ErrInvalidCarveout)
}

// The tracker sees a full Start→change→end lifecycle on success, with no error
// reported. Gated to Linux for the same reason as the assembly test: success is
// only reachable where the wall can actually be built.
func TestUnit_Command_EmitsTrackerLifecycleOnSuccess(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("a successful Command assembly is only reachable on Linux")
	}
	tr := &fakeTracker{}

	_, err := libsandbox.Command(context.Background(), libsandbox.Spec{
		WorkspaceRoot: t.TempDir(),
		Home:          t.TempDir(),
		Tracker:       tr,
	}, "true")

	require.NoError(t, err)
	require.Equal(t, 1, tr.starts)
	require.Equal(t, 1, tr.changes)
	require.Equal(t, 1, tr.ends)
	require.Empty(t, tr.errs)
}

// A failure still ends the tracked operation and reports the error to the
// tracker rather than swallowing it.
func TestUnit_Command_ReportsErrorToTracker(t *testing.T) {
	tr := &fakeTracker{}

	_, err := libsandbox.Command(context.Background(),
		libsandbox.Spec{Home: "/h", Tracker: tr}, "true")

	require.Error(t, err)
	require.Len(t, tr.errs, 1)
	require.Equal(t, 0, tr.changes)
	require.Equal(t, 1, tr.ends)
}

// Off Linux the OS-level wall cannot be built, so a spec that is otherwise valid
// must FAIL CLOSED — Command returns (nil, ErrIsolation), never a runnable
// command carrying the real agent binary with zero confinement. This is the
// portable half of the fail-closed guarantee; the Linux enforcement is proved by
// the //go:build linux integration suite. On Linux there is nothing to assert
// here (the wall IS built), so it skips.
func TestUnit_Command_FailsClosedOffLinux(t *testing.T) {
	if runtime.GOOS == "linux" {
		t.Skip("on Linux the wall is built; the off-Linux fail-closed path does not apply")
	}
	cmd, err := libsandbox.Command(context.Background(), libsandbox.Spec{
		WorkspaceRoot: t.TempDir(),
		Home:          t.TempDir(),
	}, "true")

	require.Nil(t, cmd, "no command may be returned when the wall cannot be built")
	require.ErrorIs(t, err, libsandbox.ErrIsolation)
}

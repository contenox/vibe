//go:build linux

package agenthost

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/contenox/beam/internal/libsandbox"
	"github.com/contenox/beam/internal/store/runtimetypes"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

// This file makes the agenthost test binary a valid sandbox-shim host and smoke-
// tests the one spawn path. It is load-bearing for the WHOLE package's e2e suite,
// not just the smoke test: buildAgentCmd re-exec's THIS binary as the sandbox
// shim (libsandbox re-exec's /proc/self/exe), so without a TestMain that calls
// ShimMain at the top of main, the re-exec would just re-run `go test` instead of
// building the wall and exec'ing the confined agent. TestMain here mirrors
// cmd/contenox/main.go so every spawning test — stub, testy, loopback — confines
// correctly.

const (
	// smokeProbeEnv, when set in the confined child, switches this test binary
	// into its sandbox probe: instead of running the suite it performs one
	// confinement check and exits by code. It travels as EnvSet (buildAgentCmd's
	// Spec), which survives libsandbox's env scrub, and the shim strips only its
	// own transport vars — so it reaches the confined process.
	smokeProbeEnv = "CONTENOX_AGENTHOST_SANDBOX_PROBE"

	exitProbeUnreachable = 20 // confined: outbound connect refused, net-unreachable (wall holds)
	exitProbeConnected   = 21 // NOT confined: outbound connect succeeded (wall breached)
	exitProbeOther       = 22 // outbound connect failed some other way
	exitShimFailed       = 5  // the sandbox shim itself failed before it could exec the target
)

// TestMain wires this test binary as a sandbox-shim host (see file doc) and, on
// an ordinary run, dispatches the confinement probe layer before the suite.
func TestMain(m *testing.M) {
	// Layer 1: re-exec'd by libsandbox.Command as the sandbox shim. On success
	// ShimMain builds the wall and execve's the target and never returns; a
	// return means it failed and we must not fall through.
	if handled, err := libsandbox.ShimMain(); handled {
		if err != nil {
			fmt.Fprintln(os.Stderr, "agenthost sandbox shim:", err)
			os.Exit(exitShimFailed)
		}
		os.Exit(0) // unreachable: a successful shim already execve'd the target
	}
	// Layer 2: execve'd by the shim, now confined. Run the one probe and exit.
	if os.Getenv(smokeProbeEnv) != "" {
		os.Exit(runSmokeProbe())
	}
	// Ordinary test run.
	os.Exit(m.Run())
}

// runSmokeProbe performs one confinement check from inside the wall: an outbound
// TCP connection to a routable literal (192.0.2.1, TEST-NET-1, RFC 5737). In the
// empty network namespace the wall builds — no route, offline by construction —
// the kernel refuses it as network-unreachable; a success would mean the wall
// leaked. It uses an IP literal so there is no DNS lookup (there is no resolver
// in here anyway).
func runSmokeProbe() int {
	c, err := net.DialTimeout("tcp", "192.0.2.1:80", 3*time.Second)
	if err == nil {
		_ = c.Close()
		return exitProbeConnected
	}
	if errors.Is(err, unix.ENETUNREACH) || errors.Is(err, unix.EHOSTUNREACH) {
		return exitProbeUnreachable
	}
	return exitProbeOther
}

// TestSmoke_SandboxConfinesSpawnedAgent exercises the whole spawn seam end to
// end: it asks buildAgentCmd to confine a trivial "agent" (this test binary,
// re-exec'd as the probe), runs it, and asserts from the probe's exit code that
// the agent really landed inside the wall — an outbound connection is
// network-unreachable. It proves buildAgentCmd + ShimMain confine what they
// spawn, without depending on any real ACP agent. Skipped where the kernel
// cannot build the wall.
func TestSmoke_SandboxConfinesSpawnedAgent(t *testing.T) {
	if !sandboxSupported() {
		t.Skip("sandbox unavailable: needs landlock + unprivileged user/network namespaces")
	}
	// Hermetic + offline: point HOME at an empty temp dir so buildAgentCmd finds
	// no ~/.contenox/sandbox-carveouts.json and therefore grants no network — the
	// empty netns must make any outbound connect unreachable.
	t.Setenv("HOME", t.TempDir())
	// The namespaced network wall is OPT-IN (it needs unprivileged user namespaces
	// a host may withhold; see buildAgentCmd), and with no carve-out file to imply
	// it, this operator toggle is what turns it on. Without it the default fence
	// confines the filesystem, exec, and env but leaves the host network — and this
	// test is specifically about the network half, so it opts in explicitly.
	t.Setenv("CONTENOX_SANDBOX_NETWORK_WALL", "1")

	a := &ExternalACPAgent{Config: runtimetypes.ExternalACPConfig{
		Transport: runtimetypes.ExternalACPTransportStdio,
		// A COPY of this test binary, not /proc/self/exe: a command that resolves to
		// the running executable is contenox spawning contenox and is deliberately not
		// confined (selfInvocation), which would make this test assert nothing. The
		// copy is a distinct file, so it is a foreign agent as far as the spawn path is
		// concerned, while still being a binary that knows how to run the probe.
		Command: copyOfTestBinary(t),
		Cwd:     t.TempDir(),
		Env:     map[string]string{smokeProbeEnv: "net-connect"},
	}}

	cmd, err := buildAgentCmd(context.Background(), a)
	require.NoError(t, err)

	// buildAgentCmd re-exec's /proc/self/exe as the sandbox shim (ShimMain in
	// TestMain), which builds the wall and execve's the target — the copy of this
	// binary — with smokeProbeEnv surviving the scrub, so it runs runSmokeProbe
	// confined.
	err = cmd.Run()
	require.Equal(t, exitProbeUnreachable, exitCode(err),
		"the spawned agent must be confined: an outbound connection is network-unreachable inside the empty netns")
}

// copyOfTestBinary copies the running test binary into t.TempDir() and returns the
// copy's path: a program identical in behaviour to this one (so it can run the
// probe) but a different file on disk (so the spawn path sees a foreign agent to
// confine rather than contenox re-invoking itself).
func copyOfTestBinary(t *testing.T) string {
	t.Helper()
	self, err := os.Executable()
	require.NoError(t, err)
	src, err := os.ReadFile(self)
	require.NoError(t, err)
	dst := filepath.Join(t.TempDir(), "fake-agent")
	require.NoError(t, os.WriteFile(dst, src, 0o755))
	return dst
}

// sandboxSupported reports whether this host can build the wall: a usable
// Landlock filesystem ABI and an unprivileged user+network namespace clone.
func sandboxSupported() bool {
	return landlockSupported() && usernsNetnsSupported()
}

// landlockSupported reports whether the running kernel exposes a usable Landlock
// filesystem ABI.
func landlockSupported() bool {
	r, _, e := unix.Syscall(unix.SYS_LANDLOCK_CREATE_RULESET, 0, 0, uintptr(unix.LANDLOCK_CREATE_RULESET_VERSION))
	return e == 0 && int(r) >= 1
}

// usernsNetnsSupported reports whether this host lets an unprivileged process
// create the exact user+network namespace clone the sandbox uses, by attempting
// it with /bin/true. It is more reliable than reading the sysctls, which do not
// capture an AppArmor restriction (e.g. Ubuntu 24.04).
func usernsNetnsSupported() bool {
	cmd := exec.Command("/bin/true")
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Cloneflags:                 unix.CLONE_NEWUSER | unix.CLONE_NEWNET,
		UidMappings:                []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getuid(), Size: 1}},
		GidMappings:                []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getgid(), Size: 1}},
		GidMappingsEnableSetgroups: false,
	}
	return cmd.Run() == nil
}

// exitCode extracts a process exit code from a cmd.Run/Wait error (0 on nil, the
// ExitError's code, -1 for a non-exit error).
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

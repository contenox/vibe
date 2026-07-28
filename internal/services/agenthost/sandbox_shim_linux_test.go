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

// This file makes the agenthost test binary a valid sandbox-shim host: without
// TestMain calling ShimMain, buildAgentCmd's re-exec of /proc/self/exe would
// just re-run `go test` instead of building the wall. Load-bearing for the suite.

const (
	// smokeProbeEnv switches the confined child into its sandbox probe.
	smokeProbeEnv = "CONTENOX_AGENTHOST_SANDBOX_PROBE"

	exitProbeUnreachable = 20 // confined: outbound connect refused
	exitProbeConnected   = 21 // not confined: outbound connect succeeded
	exitProbeOther       = 22 // outbound connect failed some other way
	exitShimFailed       = 5  // sandbox shim failed before exec'ing the target
)

// TestMain wires this binary as a sandbox-shim host and dispatches the probe.
func TestMain(m *testing.M) {
	// Re-exec'd as the shim; on success it execve's the target and never returns.
	if handled, err := libsandbox.ShimMain(); handled {
		if err != nil {
			fmt.Fprintln(os.Stderr, "agenthost sandbox shim:", err)
			os.Exit(exitShimFailed)
		}
		os.Exit(0) // unreachable: a successful shim already execve'd the target
	}
	// Execve'd by the shim, now confined: run the probe and exit.
	if os.Getenv(smokeProbeEnv) != "" {
		os.Exit(runSmokeProbe())
	}
	os.Exit(m.Run())
}

// runSmokeProbe checks confinement by dialing a routable IP literal: in the
// empty netns the wall builds, this must be refused as network-unreachable.
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

// TestSmoke_SandboxConfinesSpawnedAgent pins that buildAgentCmd + ShimMain
// confine a spawned agent: an outbound connection is unreachable.
func TestSmoke_SandboxConfinesSpawnedAgent(t *testing.T) {
	if !sandboxSupported() {
		t.Skip("sandbox unavailable: needs landlock + unprivileged user/network namespaces")
	}
	// Empty HOME means no carve-out file is found, so no network is granted.
	t.Setenv("HOME", t.TempDir())
	// The network wall is opt-in (see buildAgentCmd); opt in explicitly.
	t.Setenv("CONTENOX_SANDBOX_NETWORK_WALL", "1")

	a := &ExternalACPAgent{Config: runtimetypes.ExternalACPConfig{
		Transport: runtimetypes.ExternalACPTransportStdio,
		// A copy, not /proc/self/exe: the executable itself would trip selfInvocation.
		Command: copyOfTestBinary(t),
		Cwd:     t.TempDir(),
		Env:     map[string]string{smokeProbeEnv: "net-connect"},
	}}

	cmd, err := buildAgentCmd(context.Background(), a)
	require.NoError(t, err)

	// smokeProbeEnv survives the env scrub, so the confined copy runs runSmokeProbe.
	err = cmd.Run()
	require.Equal(t, exitProbeUnreachable, exitCode(err),
		"the spawned agent must be confined: an outbound connection is network-unreachable inside the empty netns")
}

// copyOfTestBinary copies the running test binary into t.TempDir(): a
// distinct file so the spawn path treats it as foreign.
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

// sandboxSupported reports whether this host can build the wall.
func sandboxSupported() bool {
	return landlockSupported() && usernsNetnsSupported()
}

// landlockSupported reports whether the kernel exposes a usable Landlock ABI.
func landlockSupported() bool {
	r, _, e := unix.Syscall(unix.SYS_LANDLOCK_CREATE_RULESET, 0, 0, uintptr(unix.LANDLOCK_CREATE_RULESET_VERSION))
	return e == 0 && int(r) >= 1
}

// usernsNetnsSupported reports whether this host allows the sandbox's clone.
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

// exitCode extracts a process exit code from a cmd.Run/Wait error.
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

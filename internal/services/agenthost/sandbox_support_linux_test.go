//go:build linux

package agenthost_test

import (
	"os"
	"os/exec"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
)

// requireSandboxable skips t unless this host can build the wall libsandbox puts
// around every spawned external ACP agent: a usable Landlock filesystem ABI AND
// an unprivileged user+network namespace clone. Since the sandbox is the ONLY
// spawn path, a test that actually spawns an agent goes through it and must skip
// gracefully where the kernel cannot enforce the wall, rather than fail.
func requireSandboxable(t *testing.T) {
	t.Helper()
	if !landlockSupported() {
		t.Skip("sandbox unavailable: landlock filesystem ABI not present on this kernel")
	}
	if !usernsNetnsSupported() {
		t.Skip("sandbox unavailable: unprivileged user+network namespaces not permitted on this host")
	}
}

// landlockSupported reports whether the running kernel exposes a usable Landlock
// filesystem ABI.
func landlockSupported() bool {
	r, _, e := unix.Syscall(unix.SYS_LANDLOCK_CREATE_RULESET, 0, 0, uintptr(unix.LANDLOCK_CREATE_RULESET_VERSION))
	return e == 0 && int(r) >= 1
}

// usernsNetnsSupported reports whether this host lets an unprivileged process
// create the exact user+network namespace clone the sandbox uses, by attempting
// it with /bin/true — more reliable than reading the sysctls, which miss an
// AppArmor restriction (e.g. Ubuntu 24.04).
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

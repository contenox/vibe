//go:build linux

package agenthost_test

import (
	"os"
	"os/exec"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
)

// requireSandboxable skips t unless this host can build the sandbox wall —
// the only spawn path, so a spawning test skips rather than fails here.
func requireSandboxable(t *testing.T) {
	t.Helper()
	if !landlockSupported() {
		t.Skip("sandbox unavailable: landlock filesystem ABI not present on this kernel")
	}
	if !usernsNetnsSupported() {
		t.Skip("sandbox unavailable: unprivileged user+network namespaces not permitted on this host")
	}
}

// landlockSupported reports whether the kernel exposes a usable Landlock ABI.
func landlockSupported() bool {
	r, _, e := unix.Syscall(unix.SYS_LANDLOCK_CREATE_RULESET, 0, 0, uintptr(unix.LANDLOCK_CREATE_RULESET_VERSION))
	return e == 0 && int(r) >= 1
}

// usernsNetnsSupported reports whether this host allows the sandbox's
// namespace clone — more reliable than reading sysctls, which miss AppArmor.
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

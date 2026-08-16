//go:build linux

package libsandbox

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"

	"github.com/contenox/contenox/libtracker"
	"golang.org/x/sys/unix"
)

func applyIsolation(ctx context.Context, cmd *exec.Cmd, spec Spec, tracker libtracker.ActivityTracker) error {
	plan, err := buildPlan(spec, cmd.Path, cmd.Args)
	if err != nil {
		return err
	}

	if spec.NetworkWall {
		if err := preflightUserns(); err != nil {
			return err
		}

		// No Net carve-outs means no egress; the netns stays routeless.
		if len(spec.Net) > 0 {
			sockFD, eerr := setupEgress(ctx, cmd, spec, tracker)
			if eerr != nil {
				return eerr
			}
			plan.Egress = true
			plan.EgressSockFD = sockFD
		}
	}

	// Fails closed if the kernel lacks seccomp user-notify.
	if spec.SyscallTap {
		if !seccompUserNotifSupported() {
			return fmt.Errorf("%w: SyscallTap was requested but this kernel has no seccomp "+
				"user-notify (SECCOMP_RET_USER_NOTIF / kernel 5.0+); the telemetry tap cannot "+
				"be built, so the wall is refused rather than run unobserved", ErrIsolation)
		}
		sockFD, terr := setupSyscallTap(ctx, cmd, tracker)
		if terr != nil {
			return terr
		}
		plan.Tap = true
		plan.TapSockFD = sockFD
	}

	blob, err := json.Marshal(plan)
	if err != nil {
		return fmt.Errorf("%w: serialize isolation plan: %v", ErrIsolation, err)
	}

	cmd.Path = shimSelfExe
	cmd.Env = append(cmd.Env, envShimSentinel+"=1", envShimSpec+"="+string(blob))
	if plan.Egress {
		// Advisory hint only; enforcement intercepts every :53 datagram regardless of destination.
		cmd.Env = append(cmd.Env, egressResolverEnv+"="+egressGatewayIP+":"+strconv.Itoa(egressDNSPort))
	}

	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
	cmd.SysProcAttr.Pdeathsig = syscall.SIGKILL

	// uid/gid map to 0 so execve regrants CAP_NET_ADMIN for the "lo" ioctl;
	// GidMappingsEnableSetgroups must be false for an unprivileged gid mapping.
	if spec.NetworkWall {
		cmd.SysProcAttr.Cloneflags |= unix.CLONE_NEWUSER | unix.CLONE_NEWNET
		cmd.SysProcAttr.UidMappings = []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getuid(), Size: 1}}
		cmd.SysProcAttr.GidMappings = []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getgid(), Size: 1}}
		cmd.SysProcAttr.GidMappingsEnableSetgroups = false
	}

	// No PID namespace: cross-userns ptrace is refused and /proc is not granted.
	return nil
}

func preflightUserns() error {
	if v, err := os.ReadFile("/proc/sys/kernel/unprivileged_userns_clone"); err == nil {
		if strings.TrimSpace(string(v)) == "0" {
			return fmt.Errorf("%w: unprivileged user namespaces are disabled "+
				"(kernel.unprivileged_userns_clone=0) — the network wall needs an "+
				"unprivileged user+network namespace; enable it "+
				"(sysctl -w kernel.unprivileged_userns_clone=1) or host the agent "+
				"where userns is permitted", ErrIsolation)
		}
	}
	if v, err := os.ReadFile("/proc/sys/user/max_user_namespaces"); err == nil {
		if strings.TrimSpace(string(v)) == "0" {
			return fmt.Errorf("%w: user namespaces are disabled "+
				"(user.max_user_namespaces=0) — the network wall needs an "+
				"unprivileged user+network namespace", ErrIsolation)
		}
	}
	return nil
}

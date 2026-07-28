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

	"github.com/contenox/beam/internal/libtracker"
	"golang.org/x/sys/unix"
)

// applyIsolation builds the deny-by-construction wall on Linux by rewriting cmd
// to re-exec the host binary as a sandbox shim instead of the target directly:
// Landlock must be applied in the child before execve of the real program (the
// parent can't apply it to itself), and Go cannot run code between fork and
// exec, so the shim applies the wall from ShimMain and only then execs the agent.
//
// cmd.Path becomes /proc/self/exe; cmd.Args is left as the caller's argv (only
// the shim's cosmetic argv0). The real target, its argv, and the resolved plan
// are serialized into two transport env vars appended to cmd.Env; the shim
// strips them before exec. SysProcAttr gets Pdeathsig=SIGKILL and Setpgid.
//
// With spec.NetworkWall, the child is cloned into a fresh CLONE_NEWUSER|
// CLONE_NEWNET namespace pair (see below). No mount/pid/ipc/uts namespaces:
// Landlock already jails the filesystem, and a mount-ns would break the
// read-only runtime grant the dynamically linked agent needs to load.
//
// Fail-closed: namespace creation happens via clone flags, so there is no path
// where the child runs with the network open — a failed clone means the child
// never starts. Errors wrap ErrInvalidSpec (bad workspace/home) or ErrIsolation.
func applyIsolation(ctx context.Context, cmd *exec.Cmd, spec Spec, tracker libtracker.ActivityTracker) error {
	plan, err := buildPlan(spec, cmd.Path, cmd.Args)
	if err != nil {
		return err
	}

	// Every userns/netns/egress step lives behind spec.NetworkWall; off, the
	// agent keeps the host network and the fs/exec/env fence builds with zero
	// namespace privilege, so it works on hosts with unprivileged userns disabled.
	if spec.NetworkWall {
		if err := preflightUserns(); err != nil {
			return err
		}

		// Metered egress: wire the parent-side bridge and tell the shim (via the
		// plan) to create the TUN. No carve-outs means no egress; the netns keeps
		// the deny-by-construction floor. setupEgress's goroutine is bound to ctx.
		if len(spec.Net) > 0 {
			sockFD, eerr := setupEgress(ctx, cmd, spec, tracker)
			if eerr != nil {
				return eerr
			}
			plan.Egress = true
			plan.EgressSockFD = sockFD
		}
	}

	// Syscall telemetry tap (opt-in, independent of egress). Fail closed up front
	// if the kernel lacks user-notify, rather than silently downgrading to no
	// telemetry. setupSyscallTap's goroutine is bound to ctx.
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
		// Advisory resolver hint only; enforcement intercepts every :53 datagram
		// regardless of destination.
		cmd.Env = append(cmd.Env, egressResolverEnv+"="+egressGatewayIP+":"+strconv.Itoa(egressDNSPort))
	}

	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
	cmd.SysProcAttr.Pdeathsig = syscall.SIGKILL

	// CLONE_NEWNET makes the network absent by construction; CLONE_NEWUSER lets an
	// unprivileged process own that netns. uid/gid map to 0 in the userns so
	// execve regrants CAP_NET_ADMIN to raise "lo" (a same-uid mapping would leave
	// the post-execve shim EPERM on the ioctl); HostID keeps on-disk ownership at
	// the real uid/gid. GidMappingsEnableSetgroups must be false for an
	// unprivileged gid mapping to be accepted.
	if spec.NetworkWall {
		cmd.SysProcAttr.Cloneflags |= unix.CLONE_NEWUSER | unix.CLONE_NEWNET
		cmd.SysProcAttr.UidMappings = []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getuid(), Size: 1}}
		cmd.SysProcAttr.GidMappings = []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getgid(), Size: 1}}
		cmd.SysProcAttr.GidMappingsEnableSetgroups = false
	}

	// Accepted limitation: no PID namespace, so the agent shares the host pid
	// namespace and can signal the operator's other same-uid processes, including
	// this supervisor (a SIGSTOP would freeze the tap/bridge goroutines without
	// killing them, since Pdeathsig fires only on supervisor death). Cross-userns
	// ptrace is refused by the kernel and /proc is not Landlock-granted, bounding
	// the exposure; adding a PID namespace would require the shim to become a
	// reaping pid-1 init, out of scope here.
	return nil
}

// preflightUserns fails closed, up front, when unprivileged user namespaces are
// disabled, reporting ErrIsolation instead of an opaque clone EPERM at
// cmd.Start(). Best-effort: checks kernel.unprivileged_userns_clone (Debian/
// Ubuntu-only knob, absence is not a failure) and user.max_user_namespaces==0.
// Not exhaustive (e.g. AppArmor restrictions aren't caught here), but the wall
// still fails closed regardless since CLONE_NEWNET is a clone flag.
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

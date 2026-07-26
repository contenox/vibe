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
// to re-exec the *host binary as a sandbox shim* instead of the target directly.
// This indirection is load-bearing: Landlock must be applied in the child,
// before execve of the real program (applying it in the parent would restrict
// contenox itself), and Go cannot run code between fork and exec — so the child
// is our own binary, which applies the wall from ShimMain and only then execs
// the agent. See docs/development/blueprints/acp/agent-sandbox.md §2.2 and §5.
//
// What it changes, and what it deliberately does not:
//   - cmd.Path becomes /proc/self/exe (the host binary). cmd.Args is left as the
//     caller's argv — it is only the shim's cosmetic argv0, and keeping it means
//     Command's returned cmd still reports the caller's arguments unchanged.
//   - The real target (cmd.Path before this call), its argv, and the resolved
//     plan (workspace, "~"-resolved carve-outs, scoped home) are serialized and
//     handed to the shim through two transport env vars appended to the already
//     scrubbed, HOME-scoped cmd.Env. The shim strips them before exec, so the
//     agent never sees them.
//   - SysProcAttr gets Pdeathsig=SIGKILL (die with the supervisor) and Setpgid
//     (confine the whole agent → Bash → git tree to one group), matching the
//     libprocess supervision idiom.
//   - SysProcAttr also gets CLONE_NEWUSER|CLONE_NEWNET (setgroups disabled), so
//     the child is cloned straight into a fresh, empty network namespace — no
//     external interface, no route — the deny-by-construction network wall
//     (blueprint §2.2 net). The unprivileged userns is what makes the netns
//     creatable without root and is what the netns is owned by. The current
//     uid/gid are mapped to 0 INSIDE the userns (root-in-namespace): the shim
//     needs CAP_NET_ADMIN over its netns to raise "lo", but the re-exec to the
//     shim is an execve, and execve as a non-root-in-userns uid drops every
//     capability — so a same→same mapping would leave the shim powerless (EPERM
//     on the "lo"-up ioctl). Mapping to userns-root means execve regrants full
//     caps within the userns, so the shim can bring "lo" up; on-disk ownership
//     of the agent's workspace writes still resolves to the real host uid/gid
//     (the mapping's HostID), so contenox still owns what the agent creates. The
//     caps are confined to the userns and cannot touch the host. Mount/pid/ipc/
//     uts namespaces are deliberately NOT added — Landlock already jails the
//     filesystem, and adding a mount-ns here would break the read-only system
//     runtime grant the dynamically linked agent needs to load.
//
// The namespaces are created at clone (here, parent-side) before ShimMain runs,
// so they compose with the child-applied Landlock: the shim lands already inside
// the new user+net namespaces, then builds the fs wall over the unchanged host
// mounts. The Landlock ruleset itself is not built here — it is built in the
// child by ShimMain, because that is the only place it can be applied without
// also jailing the parent. All mechanisms are CGo-free (golang.org/x/sys/unix +
// SysProcAttr).
//
// Fail-closed: network isolation is applied via clone flags, not a post-start
// syscall, so there is no code path where the child runs with the network open —
// if the namespace cannot be created the child does not start at all. Where the
// most common cause (a host with unprivileged user namespaces disabled) is
// cheaply detectable it is surfaced up front as ErrIsolation with a clear
// message, rather than deferred to an opaque clone EPERM at cmd.Start(). Errors
// wrap ErrInvalidSpec (a non-absolute workspace/home) or ErrIsolation.
func applyIsolation(ctx context.Context, cmd *exec.Cmd, spec Spec, tracker libtracker.ActivityTracker) error {
	plan, err := buildPlan(spec, cmd.Path, cmd.Args)
	if err != nil {
		return err
	}

	// The namespaced network wall is the ONLY part of the wall that needs the
	// unprivileged user namespace, so every userns/netns/egress step lives behind
	// spec.NetworkWall. With it off (the default) the agent keeps the host network:
	// no preflight, no netns, no egress bridge — and the fs/exec/env fence below is
	// applied with zero namespace privilege, so it builds on hosts where
	// unprivileged userns is disabled. See Spec.NetworkWall.
	if spec.NetworkWall {
		if err := preflightUserns(); err != nil {
			return err
		}

		// Metered egress: when the spec names network carve-outs, wire the parent-side
		// bridge and tell the shim (via the plan) to create the TUN and where to hand
		// it over. With no carve-outs this is skipped and the netns keeps the
		// deny-by-construction floor. setupEgress launches a goroutine bound to ctx, so
		// egress lives exactly as long as the assembly's context; a cancelled ctx tears
		// the stack down.
		if len(spec.Net) > 0 {
			sockFD, eerr := setupEgress(ctx, cmd, spec, tracker)
			if eerr != nil {
				return eerr
			}
			plan.Egress = true
			plan.EgressSockFD = sockFD
		}
	}

	// The syscall telemetry tap (opt-in, independent of egress): wire the parent-side
	// supervisor and tell the shim (via the plan) to install the seccomp user-notify
	// filter and where to hand the notify fd over. Fail closed and up front if the
	// kernel cannot do user-notify at all — a caller that asked to watch the wall
	// gets an honest error, not a silent downgrade to no telemetry (mirroring
	// preflightUserns). setupSyscallTap launches a goroutine bound to ctx, so the
	// supervisor lives exactly as long as the assembly's context.
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
		// Advisory resolver hint for a well-behaved agent. Enforcement does not
		// depend on it — the stack intercepts every :53 datagram regardless of
		// destination — but pointing the resolver here keeps queries on the fast
		// path. Set through the ordinary env, stripped of nothing by the shim.
		cmd.Env = append(cmd.Env, egressResolverEnv+"="+egressGatewayIP+":"+strconv.Itoa(egressDNSPort))
	}

	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
	cmd.SysProcAttr.Pdeathsig = syscall.SIGKILL

	// The network wall (NetworkWall only): clone the child into a fresh, empty
	// network namespace. CLONE_NEWNET makes the network absent by construction;
	// CLONE_NEWUSER is what lets an unprivileged process create that netns and own
	// it. The current uid/gid map to 0 inside the userns so the shim, after the
	// re-exec, is root-in-namespace and regains CAP_NET_ADMIN to raise "lo" — a
	// same→same (non-root) mapping would leave the post-execve shim with no
	// capabilities and the "lo"-up ioctl would fail EPERM. HostID is the real
	// uid/gid, so the agent's workspace writes are still owned by contenox on disk,
	// and the caps are scoped to this userns and cannot reach the host.
	// GidMappingsEnableSetgroups must be false for an unprivileged gid mapping to
	// be accepted. With NetworkWall off there are no namespaces: the child is an
	// ordinary process (real uid/gid, host network) confined by Landlock alone.
	if spec.NetworkWall {
		cmd.SysProcAttr.Cloneflags |= unix.CLONE_NEWUSER | unix.CLONE_NEWNET
		cmd.SysProcAttr.UidMappings = []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getuid(), Size: 1}}
		cmd.SysProcAttr.GidMappings = []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getgid(), Size: 1}}
		cmd.SysProcAttr.GidMappingsEnableSetgroups = false
	}

	// ACCEPTED LIMITATION — no PID namespace (CLONE_NEWPID is deliberately NOT set).
	// The agent therefore shares the HOST pid namespace and, because its userns maps
	// it to the operator's real uid/gid, it can send signals to the operator's other
	// same-uid processes — including this supervisor. A kill(SIGSTOP) to the
	// supervisor would FREEZE the telemetry tap and the egress bridge while the agent
	// keeps acting (the tap/bridge goroutines live in the parent), and Pdeathsig only
	// fires when the supervisor DIES, not when it is stopped — so a stopped
	// supervisor is a real gap in observation. This is a KNOWN, ACCEPTED risk, not an
	// oversight: adding a PID namespace would require the shim to become a reaping
	// pid-1 init (reparenting and waitpid-ing the agent's whole subtree), a
	// structural change out of scope here. Two things bound the exposure and are NOT
	// gaps: ptrace of another process is blocked (the child sits in a fresh userns and
	// cross-userns ptrace is refused by the kernel), and /proc is not granted by
	// Landlock, so the agent cannot even enumerate host pids through /proc. See
	// docs/development/blueprints/acp/agent-sandbox.md §6.
	return nil
}

// preflightUserns fails closed, up front, on the most common reason the net wall
// cannot be built: a host with unprivileged user namespaces switched off. It
// reads the two kernel knobs that gate them and reports ErrIsolation naming the
// likely cause, so the operator gets "unprivileged userns disabled" instead of an
// opaque clone EPERM surfacing later at cmd.Start().
//
// It is best-effort, not exhaustive: kernel.unprivileged_userns_clone is a
// Debian/Ubuntu-only knob (absent on mainline, where userns is on by default, so
// its absence is NOT a failure); user.max_user_namespaces==0 disables them
// everywhere. Some hosts (e.g. an AppArmor restriction on Ubuntu 24.04) block the
// clone without either knob reading zero — those are not caught here, but the
// wall still fails closed, because CLONE_NEWNET is a clone flag: a blocked clone
// means the child never starts, never with the network open.
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

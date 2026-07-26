//go:build linux

package libsandbox

import (
	"encoding/json"
	"fmt"
	"os"
	"runtime"
	"strings"
	"syscall"
)

const (
	// shimSelfExe is the host binary, re-exec'd as the shim. Using /proc/self/exe
	// (not os.Args[0] or os.Executable) is robust to the binary having been moved
	// or its argv0 rewritten.
	shimSelfExe = "/proc/self/exe"

	// envShimSentinel marks a process as the sandbox shim: ShimMain acts only when
	// it is present, so an ordinary launch of the host binary is untouched.
	envShimSentinel = "CONTENOX_SANDBOX_SHIM"

	// envShimSpec carries the JSON isolationPlan across the re-exec boundary.
	envShimSpec = "CONTENOX_SANDBOX_SPEC"
)

// ShimMain is the sandbox shim entrypoint. The host binary MUST call it at the
// very top of main(), before any flag parsing or real work:
//
//	func main() {
//	    if handled, err := libsandbox.ShimMain(); handled {
//	        if err != nil {
//	            log.Fatal(err) // wall could not be built; do NOT run unconfined
//	        }
//	        return // unreachable in practice — a successful shim execve's the agent
//	    }
//	    // ... the program's normal startup ...
//	}
//
// That one call is the entire wiring contract. When the process was not re-exec'd
// as a shim (envShimSentinel absent — every normal launch) ShimMain returns
// (false, nil) immediately and main proceeds untouched. When it IS the shim it
// does not return on success: it decodes the plan, raises "lo" inside the fresh
// network namespace it was cloned into, pins the OS thread, sets NO_NEW_PRIVS,
// builds and applies the Landlock filesystem ruleset, then syscall.Exec's the
// real target — so control passes to the now-confined agent and this call never
// comes back. It returns (true, err) only when the
// confinement or the exec failed, wrapping ErrIsolation; the fail-closed
// contract is that the caller must treat that as fatal and never fall through to
// running the target unconfined.
//
// Off Linux ShimMain is a no-op that returns (false, nil): the wall's mechanisms
// are Linux-only, so there is no shim to be.
func ShimMain() (handled bool, err error) {
	if os.Getenv(envShimSentinel) == "" {
		return false, nil
	}
	// From here the process IS the shim: every path returns handled=true.
	var plan isolationPlan
	if e := json.Unmarshal([]byte(os.Getenv(envShimSpec)), &plan); e != nil {
		return true, fmt.Errorf("%w: shim: decode plan: %v", ErrIsolation, e)
	}

	// The network wall: the child was cloned into a fresh, empty netns by
	// applyIsolation (CLONE_NEWNET), so it already has no route and no external
	// interface — that part is deny-by-construction and needs nothing here. The
	// one setup step is raising "lo" so localhost binds work; it is a netns-wide
	// operation, safe before the thread is pinned. If it fails we fail closed
	// (return ErrIsolation) rather than exec the agent into a half-built wall.
	if plan.Loopback {
		if e := bringLoopbackUp(); e != nil {
			return true, fmt.Errorf("%w: shim: %w", ErrIsolation, e)
		}
	}

	// The metered egress path (when the spec declared Net carve-outs): create the
	// TUN in this netns and hand its fd to the parent, which serves an allow-listing
	// userspace stack over it. It runs here — after "lo", before Landlock (which
	// would deny /dev/net/tun) and before the thread is pinned (netns setup is
	// process-wide) — and blocks until the parent's stack is attached, so the agent
	// never runs before its only route is being served. With no carve-outs the netns
	// keeps the deny-by-construction floor: no device, no route, plan.Egress false.
	if plan.Egress {
		if e := establishEgress(plan.EgressSockFD); e != nil {
			return true, fmt.Errorf("%w: shim: %w", ErrIsolation, e)
		}
	}

	// Pin to the OS thread so NO_NEW_PRIVS, landlock_restrict_self, and execve all
	// run on one task. Landlock's domain is per-thread and execve inherits the
	// calling thread's domain while tearing down every other (unrestricted) Go
	// runtime thread — so pinning is exactly what makes the wall carry into the
	// exec'd agent, with no all-threads dance to apply it. Not unlocked: the next
	// thing this thread does is replace the whole process image.
	runtime.LockOSThread()

	// The syscall telemetry tap: install the seccomp user-notify filter on THIS
	// (now-pinned) thread and hand its notify fd to the parent supervisor, blocking
	// on the parent's readiness ack. It must run here — after LockOSThread, because a
	// seccomp filter is per-thread and only the pinned thread's filter carries across
	// execve (exactly like Landlock) — and before execve, because the tap must be in
	// place before the agent (or even this shim's own execve) makes a tapped syscall.
	// The ack orders it: the supervisor is looping on NOTIF_RECV before the shim
	// proceeds, so the tapped execve never blocks with nothing reading. Fail closed:
	// if the filter cannot be installed or handed over, refuse rather than exec the
	// agent unobserved. Skipped entirely when the tap was not requested.
	if plan.Tap {
		if e := installSyscallTap(plan.TapSockFD); e != nil {
			return true, fmt.Errorf("%w: shim: %w", ErrIsolation, e)
		}
	}

	if e := applyLandlock(plan); e != nil {
		return true, fmt.Errorf("%w: shim: %w", ErrIsolation, e)
	}

	// Hand the agent the scrubbed, HOME-scoped environment minus the two transport
	// vars — the agent must not see how it was confined.
	childEnv := stripTransportEnv(os.Environ())
	if e := syscall.Exec(plan.Exec, plan.Args, childEnv); e != nil {
		return true, fmt.Errorf("%w: shim: exec %q: %w", ErrIsolation, plan.Exec, e)
	}
	return true, nil // unreachable: a successful Exec replaced this image.
}

// stripTransportEnv removes the shim's transport variables from env so they do
// not leak into the confined agent.
func stripTransportEnv(env []string) []string {
	out := make([]string, 0, len(env))
	for _, kv := range env {
		if strings.HasPrefix(kv, envShimSentinel+"=") || strings.HasPrefix(kv, envShimSpec+"=") {
			continue
		}
		out = append(out, kv)
	}
	return out
}

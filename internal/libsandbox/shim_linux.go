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
	// shimSelfExe is the host binary, re-exec'd as the shim. /proc/self/exe (not
	// os.Args[0]/os.Executable) is robust to the binary having moved or argv0
	// rewritten.
	shimSelfExe = "/proc/self/exe"

	// envShimSentinel marks a process as the sandbox shim; ShimMain acts only
	// when present, so an ordinary launch is untouched.
	envShimSentinel = "CONTENOX_SANDBOX_SHIM"

	// envShimSpec carries the JSON isolationPlan across the re-exec boundary.
	envShimSpec = "CONTENOX_SANDBOX_SPEC"
)

// ShimMain is the sandbox shim entrypoint. The host binary must call it at the
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
// When not re-exec'd as a shim (envShimSentinel absent), ShimMain returns
// (false, nil) immediately. As the shim, it does not return on success: it
// decodes the plan, raises "lo", pins the OS thread, applies the seccomp tap
// and Landlock ruleset, then syscall.Exec's the real target. It returns
// (true, err) only on confinement/exec failure, wrapping ErrIsolation; the
// fail-closed contract is that the caller must treat that as fatal, never
// falling through to run the target unconfined.
//
// Off Linux, ShimMain is a no-op returning (false, nil).
func ShimMain() (handled bool, err error) {
	if os.Getenv(envShimSentinel) == "" {
		return false, nil
	}
	// From here the process is the shim: every path returns handled=true.
	var plan isolationPlan
	if e := json.Unmarshal([]byte(os.Getenv(envShimSpec)), &plan); e != nil {
		return true, fmt.Errorf("%w: shim: decode plan: %v", ErrIsolation, e)
	}

	// Network wall: the netns (CLONE_NEWNET, from applyIsolation) already has no
	// route/interface by construction. The one setup step is raising "lo"
	// (netns-wide, safe before thread pinning); failure fails closed.
	if plan.Loopback {
		if e := bringLoopbackUp(); e != nil {
			return true, fmt.Errorf("%w: shim: %w", ErrIsolation, e)
		}
	}

	// Metered egress (Net carve-outs): create the TUN in this netns and hand its
	// fd to the parent's userspace stack. Runs after "lo", before Landlock
	// (which would deny /dev/net/tun) and before thread pinning (netns setup is
	// process-wide); blocks until the parent's stack attaches, so the agent
	// never runs before its only route is served.
	if plan.Egress {
		if e := establishEgress(plan.EgressSockFD); e != nil {
			return true, fmt.Errorf("%w: shim: %w", ErrIsolation, e)
		}
	}

	// Pin to the OS thread so NO_NEW_PRIVS, landlock_restrict_self, and execve
	// all run on one task: Landlock's domain is per-thread, and execve inherits
	// the calling thread's domain while tearing down every other Go runtime
	// thread. Never unlocked — the next thing this thread does is exec.
	runtime.LockOSThread()

	// Syscall telemetry tap: install the seccomp user-notify filter on this
	// (now-pinned) thread and hand its fd to the parent, blocking on the
	// readiness ack. Must run after LockOSThread (a seccomp filter is per-thread,
	// like Landlock) and before execve so the tap is in place before any tapped
	// syscall. Fail closed if the filter cannot be installed or handed over.
	if plan.Tap {
		if e := installSyscallTap(plan.TapSockFD); e != nil {
			return true, fmt.Errorf("%w: shim: %w", ErrIsolation, e)
		}
	}

	if e := applyLandlock(plan); e != nil {
		return true, fmt.Errorf("%w: shim: %w", ErrIsolation, e)
	}

	// Strip the transport vars — the agent must not see how it was confined.
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

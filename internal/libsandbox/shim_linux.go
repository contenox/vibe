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
	shimSelfExe = "/proc/self/exe"

	envShimSentinel = "CONTENOX_SANDBOX_SHIM"

	envShimSpec = "CONTENOX_SANDBOX_SPEC"
)

// ShimMain is the sandbox shim entrypoint, called at the top of main(). It
// returns (false, nil) when not invoked as the shim; otherwise it does not
// return except with (true, err) wrapping ErrIsolation, which is fatal.
func ShimMain() (handled bool, err error) {
	if os.Getenv(envShimSentinel) == "" {
		return false, nil
	}
	var plan isolationPlan
	if e := json.Unmarshal([]byte(os.Getenv(envShimSpec)), &plan); e != nil {
		return true, fmt.Errorf("%w: shim: decode plan: %v", ErrIsolation, e)
	}

	// netns setup is process-wide, so raising "lo" before thread pinning is safe.
	if plan.Loopback {
		if e := bringLoopbackUp(); e != nil {
			return true, fmt.Errorf("%w: shim: %w", ErrIsolation, e)
		}
	}

	// Must run after "lo", before Landlock (which denies /dev/net/tun) and before thread pinning (netns setup is process-wide).
	if plan.Egress {
		if e := establishEgress(plan.EgressSockFD); e != nil {
			return true, fmt.Errorf("%w: shim: %w", ErrIsolation, e)
		}
	}

	// Landlock's domain is per-thread and execve inherits only the calling
	// thread's domain, so the rest must run pinned to one thread.
	runtime.LockOSThread()

	// Seccomp is per-thread like Landlock: after LockOSThread, before execve.
	if plan.Tap {
		if e := installSyscallTap(plan.TapSockFD); e != nil {
			return true, fmt.Errorf("%w: shim: %w", ErrIsolation, e)
		}
	}

	if e := applyLandlock(plan); e != nil {
		return true, fmt.Errorf("%w: shim: %w", ErrIsolation, e)
	}

	childEnv := stripTransportEnv(os.Environ())
	if e := syscall.Exec(plan.Exec, plan.Args, childEnv); e != nil {
		return true, fmt.Errorf("%w: shim: exec %q: %w", ErrIsolation, plan.Exec, e)
	}
	return true, nil // unreachable: a successful Exec replaced this image.
}

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

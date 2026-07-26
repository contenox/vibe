//go:build !linux

package libsandbox

import (
	"context"
	"fmt"
	"os/exec"

	"github.com/contenox/beam/internal/libtracker"
)

// applyIsolation FAILS CLOSED off Linux. The wall's enforcement mechanisms —
// Landlock and the network/mount namespaces — are Linux-only, and macOS parity
// (a weaker sandbox-exec backend) is explicitly out of v1 scope (blueprint §6).
// Command still scrubs the environment and pins Dir and HOME on every platform,
// so the credential-leak fix is portable; but the deny-by-construction wall
// cannot be built here, so it would be dangerous to hand a caller a *runnable*
// command with the REAL agent binary on cmd.Path and zero confinement — a caller
// that reasonably reads "nil err ⇒ confined" would run the agent wide open.
//
// So instead of returning nil (the old no-op, which leaked exactly that runnable
// unconfined command), this returns an ErrIsolation error. Command then returns
// (nil, err) on every non-Linux host, and the fail-closed contract holds
// everywhere: no wall, no command. A caller that genuinely wants an unconfined
// run on a non-Linux host must build a bare exec.Command itself, not route
// through libsandbox.
//
// The egress bridge (network carve-outs → allow-listing userspace stack) is part
// of the same Linux-only seam: off Linux it is absent, so ctx and tracker are
// unused here. The allow-list/DNS decision core (egresspolicy.go) still compiles
// on every platform, ready to drive an advisory path later, but nothing invokes
// it here.
func applyIsolation(ctx context.Context, cmd *exec.Cmd, spec Spec, tracker libtracker.ActivityTracker) error {
	return fmt.Errorf("%w: OS-level confinement is implemented only on Linux; "+
		"refusing to hand back an unconfined command (build a bare exec.Command "+
		"yourself if an unconfined run is genuinely intended)", ErrIsolation)
}

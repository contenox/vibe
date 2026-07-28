//go:build !linux

package libsandbox

import (
	"context"
	"fmt"
	"os/exec"

	"github.com/contenox/contenox/internal/libtracker"
)

// applyIsolation fails closed off Linux: Landlock and the network/mount
// namespaces are Linux-only, so the deny-by-construction wall cannot be
// built here. Returning an error (rather than the old no-op) means Command
// returns (nil, err) on every non-Linux host instead of handing back a
// runnable-but-unconfined command. A caller that genuinely wants an
// unconfined run must build a bare exec.Command itself.
//
// ctx and tracker are unused: the egress bridge is part of the same
// Linux-only seam and is absent here, though egresspolicy.go's allow-list/DNS
// core still compiles on every platform.
func applyIsolation(ctx context.Context, cmd *exec.Cmd, spec Spec, tracker libtracker.ActivityTracker) error {
	return fmt.Errorf("%w: OS-level confinement is implemented only on Linux; "+
		"refusing to hand back an unconfined command (build a bare exec.Command "+
		"yourself if an unconfined run is genuinely intended)", ErrIsolation)
}

//go:build linux

package libsandbox

import "fmt"

// Preflight reports whether the confinement floor (the Landlock filesystem/exec
// wall applied to every spawned agent) can be built on this host, without
// attempting a spawn. nil means an agent can be confined here; a non-nil error
// (wrapping ErrIsolation) names why not.
//
// Checks only the floor (Landlock) on purpose: the default wall needs nothing
// else, so it builds even where unprivileged userns is disabled. The opt-in
// network wall's extra userns requirement is preflighted separately at spawn
// (preflightUserns). See Spec.NetworkWall.
func Preflight() error {
	abi, err := landlockABI()
	if err != nil {
		return fmt.Errorf("%w: cannot query Landlock support on this kernel: %v", ErrIsolation, err)
	}
	if abi < 1 {
		return fmt.Errorf("%w: this kernel exposes no usable Landlock filesystem ABI "+
			"(Landlock needs Linux 5.13+ with the LSM enabled at boot); the agent sandbox "+
			"cannot be built here", ErrIsolation)
	}
	return nil
}

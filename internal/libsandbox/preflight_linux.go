//go:build linux

package libsandbox

import "fmt"

// Preflight reports whether the confinement FLOOR — the Landlock filesystem/exec
// wall applied to every spawned agent — can be built on this host, WITHOUT
// attempting a spawn. It is the cheap, early check a caller runs before offering
// to run an external agent: refuse up front with a clear, actionable message
// instead of letting the wall's fail-closed contract surface later as an opaque
// child-side failure at cmd.Start(). nil means an agent CAN be confined here; a
// non-nil error (wrapping ErrIsolation) names why not.
//
// It checks only the floor (Landlock), on purpose. The default wall needs Landlock
// and nothing else — no user namespace, no privilege — so it builds on stock
// hosts (including those where unprivileged user namespaces are disabled). The
// opt-in network wall's extra requirement (an unprivileged userns) is preflighted
// separately at spawn (preflightUserns), because a host without userns can still
// confine the filesystem, exec, and environment — it just cannot confine the
// network. See Spec.NetworkWall and applyIsolation.
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

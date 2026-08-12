//go:build linux

package libsandbox

import "fmt"

// Preflight reports whether the Landlock confinement floor can be built on this host without attempting a spawn; nil means yes, a non-nil error (wrapping ErrIsolation) says why not.
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

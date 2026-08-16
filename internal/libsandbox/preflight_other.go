//go:build !linux

package libsandbox

import "fmt"

// Preflight always returns a non-nil error on non-Linux hosts, since
// OS-level confinement is Linux-only.
func Preflight() error {
	return fmt.Errorf("%w: OS-level agent confinement is implemented only on Linux; "+
		"the agent sandbox cannot be built on this host", ErrIsolation)
}

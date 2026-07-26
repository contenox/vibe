//go:build !linux

package libsandbox

import "fmt"

// Preflight fails on non-Linux hosts: the deny-by-construction wall (Landlock plus
// the namespace mechanisms) is Linux-only, so an external agent cannot be confined
// here and — per the fail-closed contract (see applyIsolation in
// isolation_other.go) — must not be run. Returning the error up front lets a caller
// refuse with a clear message rather than building a command that would fail at
// spawn.
func Preflight() error {
	return fmt.Errorf("%w: OS-level agent confinement is implemented only on Linux; "+
		"the agent sandbox cannot be built on this host", ErrIsolation)
}

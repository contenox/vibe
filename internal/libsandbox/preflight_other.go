//go:build !linux

package libsandbox

import "fmt"

// Preflight fails on non-Linux hosts: the deny-by-construction wall is
// Linux-only (see applyIsolation), so it reports the error up front rather
// than letting a caller build a command that would fail at spawn.
func Preflight() error {
	return fmt.Errorf("%w: OS-level agent confinement is implemented only on Linux; "+
		"the agent sandbox cannot be built on this host", ErrIsolation)
}

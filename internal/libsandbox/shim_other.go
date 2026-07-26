//go:build !linux

package libsandbox

// ShimMain is a deliberate no-op off Linux. The wall's enforcement mechanisms
// (Landlock, and the namespaces of later slices) are Linux-only, so there is no
// sandbox shim to be and nothing to re-exec into: applyIsolation does not rewrite
// the command on these platforms (see isolation_other.go). It always returns
// (false, nil), so a host binary that calls it at the top of main() runs normally
// everywhere and the one-line wiring contract is portable. A caller that requires
// the wall must still gate on runtime.GOOS — a spec "confined" here is not jailed.
func ShimMain() (handled bool, err error) {
	return false, nil
}

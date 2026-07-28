//go:build !linux

package libsandbox

// ShimMain is a deliberate no-op off Linux (nothing to re-exec into; see
// isolation_other.go), always returning (false, nil) so a host binary can
// call it unconditionally at the top of main(). A caller that requires the
// wall must still gate on runtime.GOOS — a spec "confined" here is not jailed.
func ShimMain() (handled bool, err error) {
	return false, nil
}

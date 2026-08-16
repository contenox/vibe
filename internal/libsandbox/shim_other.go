//go:build !linux

package libsandbox

// ShimMain is a no-op off Linux, always returning (false, nil):
// confinement is not enforced here regardless of Spec.
func ShimMain() (handled bool, err error) {
	return false, nil
}

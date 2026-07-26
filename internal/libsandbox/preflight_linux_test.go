//go:build linux

package libsandbox

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// On a Landlock-capable Linux host the floor preflight passes, and its verdict
// agrees with the raw ABI probe it is built on — so a caller can trust Preflight
// as the early gate for "can an external agent be confined here".
func TestUnit_Preflight_PassesWhereLandlockIsSupported(t *testing.T) {
	abi, err := landlockABI()
	if err != nil || abi < 1 {
		t.Skipf("host has no usable Landlock ABI (abi=%d err=%v); the pass path is unreachable here", abi, err)
	}

	require.NoError(t, Preflight(), "Preflight must pass on a Landlock-capable host")
}

// Whatever the verdict, a failure is always an ErrIsolation — the sentinel the
// fail-closed callers match on — never a bare error.
func TestUnit_Preflight_FailureWrapsErrIsolation(t *testing.T) {
	if err := Preflight(); err != nil {
		require.ErrorIs(t, err, ErrIsolation)
	}
}

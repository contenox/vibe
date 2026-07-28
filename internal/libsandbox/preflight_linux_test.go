//go:build linux

package libsandbox

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// Preflight passes on a Landlock-capable host, agreeing with the raw ABI probe.
func TestUnit_Preflight_PassesWhereLandlockIsSupported(t *testing.T) {
	abi, err := landlockABI()
	if err != nil || abi < 1 {
		t.Skipf("host has no usable Landlock ABI (abi=%d err=%v); the pass path is unreachable here", abi, err)
	}

	require.NoError(t, Preflight(), "Preflight must pass on a Landlock-capable host")
}

// A Preflight failure is always ErrIsolation, never a bare error.
func TestUnit_Preflight_FailureWrapsErrIsolation(t *testing.T) {
	if err := Preflight(); err != nil {
		require.ErrorIs(t, err, ErrIsolation)
	}
}

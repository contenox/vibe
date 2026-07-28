//go:build !linux

package agenthost_test

import "testing"

// requireSandboxable always skips off Linux: the sandbox wall is Linux-only,
// so a spawn there fails closed (libsandbox.Command returns ErrIsolation).
func requireSandboxable(t *testing.T) {
	t.Helper()
	t.Skip("external ACP agent hosting is Linux-only (the libsandbox wall requires Linux)")
}

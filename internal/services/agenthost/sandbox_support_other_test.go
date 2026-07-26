//go:build !linux

package agenthost_test

import "testing"

// requireSandboxable always skips off Linux. External ACP agent hosting requires
// the libsandbox wall, whose enforcement mechanisms are Linux-only, so a spawn
// there fails closed (libsandbox.Command returns ErrIsolation). A test that
// spawns an agent therefore cannot run off Linux and skips with a clear reason
// rather than fail — this is the intended, fail-closed consequence of making the
// sandbox the only spawn path.
func requireSandboxable(t *testing.T) {
	t.Helper()
	t.Skip("external ACP agent hosting is Linux-only (the libsandbox wall requires Linux)")
}

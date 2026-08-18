package contenoxcli

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// The workspace-root allowlist is gone: a host serves the one root it was
// launched with and an ACP client's cwd is authoritative, so nothing reads a
// grant any more. The verbs that wrote them must not be reachable, or they
// would keep teaching an allowlist that no longer decides anything.
func TestUnit_WorkspaceCommandIsRetired(t *testing.T) {
	for _, c := range rootCmd.Commands() {
		require.NotEqual(t, "workspace", c.Name(), "`contenox workspace` is retired and must not be registered")
	}
	require.False(t, reservedSubcommands["workspace"], `"workspace" names no command, so it reserves nothing`)
	require.False(t, firstNonFlagIsReserved([]string{"workspace", "add", "/x"}))
}

package acpsvc

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestUnit_SessionListing_ScopedToTheRequestedCwd pins the narrowing a client
// without a host root depends on. beam and an editor build no workspace-root
// factory, so sessionInWorkspaceView admits everything; if the listing does not
// also honour the requested cwd, `contenox beam .` resumes the newest session on
// the machine instead of the newest one in this directory.
func TestUnit_SessionListing_ScopedToTheRequestedCwd(t *testing.T) {
	here := t.TempDir()
	elsewhere := t.TempDir()
	nested := filepath.Join(here, "pkg")

	require.True(t, sessionUnderRequestedCwd(here, here), "a session rooted here is listed")
	require.True(t, sessionUnderRequestedCwd(nested, here), "so is one rooted below it")
	require.False(t, sessionUnderRequestedCwd(elsewhere, here),
		"a session from another workspace must not be listed, or beam resumes it")

	require.True(t, sessionUnderRequestedCwd(elsewhere, ""), "an unscoped request still sees everything")
	require.True(t, sessionUnderRequestedCwd(elsewhere, "/"), "and so does the sentinel")

	require.False(t, sessionUnderRequestedCwd("", here),
		"an untagged session is attributable to no directory, so a scoped listing omits it")
}

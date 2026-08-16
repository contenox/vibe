package runtimetypes_test

import (
	"testing"

	"github.com/contenox/contenox/internal/kernel/taskengine"
	"github.com/contenox/contenox/internal/store/runtimetypes"
	"github.com/stretchr/testify/require"
)

// The kernel is store-free by design, so taskengine carries its own copy of the
// declaration-scope prefix rather than importing this package.
func TestUnit_DeclaredPrefixMatchesTheKernelsCopy(t *testing.T) {
	scoped := runtimetypes.DeclaredToolName("reviewer", "filesystem")

	got := taskengine.ExportedApplyAllowlist([]string{"*"}, []string{"local_fs", scoped})
	require.Equal(t, []string{"local_fs"}, got,
		"a wildcard must not reach a declaration-scoped toolset — the kernel's prefix copy has drifted from runtimetypes.DeclaredToolNamePrefix")

	got = taskengine.ExportedApplyAllowlist([]string{"local_fs", scoped}, []string{"local_fs", scoped})
	require.Equal(t, []string{"local_fs", scoped}, got,
		"naming a declaration-scoped toolset exactly must still resolve — that is how its own chain reaches it")
}

func TestUnit_DeclaredToolNameIsScopedAndStable(t *testing.T) {
	first := runtimetypes.DeclaredToolName("reviewer", "filesystem")
	require.Equal(t, first, runtimetypes.DeclaredToolName("reviewer", "filesystem"),
		"the name must be stable across syncs: the emitted chain references it statically")

	require.NotEqual(t, first, runtimetypes.DeclaredToolName("triage", "filesystem"),
		"two agents declaring the same name must not collide")

	require.True(t, runtimetypes.IsDeclaredToolName(first))
	require.False(t, runtimetypes.IsDeclaredToolName("filesystem"),
		"an operator-registered server is not declaration-scoped")
	require.False(t, runtimetypes.IsACPManagedMCPServerName(first),
		"the two non-durable owners must stay distinguishable for the boot sweep")
}

package runtimetypes_test

import (
	"testing"

	"github.com/contenox/contenox/internal/kernel/taskengine"
	"github.com/contenox/contenox/internal/store/runtimetypes"
	"github.com/stretchr/testify/require"
)

// TestUnit_DeclaredNameIsAnOrdinaryAllowlistEntry pins that the decl- prefix is a namespace the kernel never reads: "*" admits a declared toolset, "!name" removes it, a bare name grants exactly it.
func TestUnit_DeclaredNameIsAnOrdinaryAllowlistEntry(t *testing.T) {
	scoped := runtimetypes.DeclaredToolName("reviewer", "filesystem")
	universe := []string{"local_fs", scoped}

	require.Equal(t, universe, taskengine.ExportedApplyAllowlist([]string{"*"}, universe),
		"\"*\" must admit a declared toolset like any other; the prefix is a namespace, not a hidden exclusion")

	require.Equal(t, []string{"local_fs"},
		taskengine.ExportedApplyAllowlist([]string{"*", "!" + scoped}, universe),
		"\"!\"+the declared name is how an operator drops exactly that toolset")

	require.Equal(t, universe, taskengine.ExportedApplyAllowlist(universe, universe),
		"naming a declared toolset exactly must resolve — that is how its own chain reaches it")

	require.Empty(t, taskengine.ExportedApplyAllowlist(nil, universe),
		"an empty allowlist grants nothing")
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

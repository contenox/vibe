// scope_test.go is the anti-regression guard for the writer/reader scope
// split: every registered workspace-scoped key must round-trip through the
// public write door and the public read door to the same value, for the same
// caller. A future key added to the registry is covered without editing this
// file; a future reader wired to the wrong scope fails it.
package clikv_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/contenox/contenox/internal/services/clikv"
	"github.com/contenox/contenox/internal/store/runtimetypes"
	libdb "github.com/contenox/contenox/libdbexec"
	"github.com/stretchr/testify/require"
)

func scopeStore(t *testing.T) (context.Context, runtimetypes.Store) {
	t.Helper()
	ctx := context.Background()
	db, err := libdb.NewSQLiteDBManager(ctx, filepath.Join(t.TempDir(), "clikv-scope.db"), runtimetypes.SchemaSQLite)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	return ctx, runtimetypes.New(db.WithoutTransaction())
}

// TestUnit_WorkspaceScopedKeys_WriterAndReaderAgree pins that WriteConfig and
// ReadConfig target one row per (key, workspace): the invariant scopeFor
// exists to hold, and the one `contenox config set hitl-policy-name` broke by
// writing a workspace row the evaluator read globally.
func TestUnit_WorkspaceScopedKeys_WriterAndReaderAgree(t *testing.T) {
	ctx, store := scopeStore(t)
	keys := clikv.WorkspaceScopedKeys()
	require.NotEmpty(t, keys, "the registry is the subject of this test")

	for _, key := range keys {
		require.True(t, clikv.IsWorkspaceScoped(key))

		require.NoError(t, clikv.WriteConfig(ctx, store, "ws-a", key, "value-a"))
		require.NoError(t, clikv.WriteConfig(ctx, store, "ws-b", key, "value-b"))

		got, scope := clikv.ReadConfig(ctx, store, "ws-a", key)
		require.Equalf(t, "value-a", got, "%s must read back what ws-a wrote", key)
		require.Equal(t, "workspace", scope)

		got, scope = clikv.ReadConfig(ctx, store, "ws-b", key)
		require.Equalf(t, "value-b", got, "%s must not leak ws-a's value into ws-b", key)
		require.Equal(t, "workspace", scope)
	}
}

// TestUnit_WorkspaceScopedKeys_EmptyWorkspaceIsTheGlobalRow pins the fallback
// leg: a caller with no workspace writes and reads the global row, and a
// workspace that never overrode the key inherits it.
func TestUnit_WorkspaceScopedKeys_EmptyWorkspaceIsTheGlobalRow(t *testing.T) {
	ctx, store := scopeStore(t)
	for _, key := range clikv.WorkspaceScopedKeys() {
		require.NoError(t, clikv.WriteConfig(ctx, store, "", key, "global-value"))

		got, scope := clikv.ReadConfig(ctx, store, "", key)
		require.Equal(t, "global-value", got)
		require.Equal(t, "global", scope)
		require.Equalf(t, "global-value", clikv.Read(ctx, store, key),
			"a workspace-less write must land in the row Read sees")

		got, scope = clikv.ReadConfig(ctx, store, "ws-fresh", key)
		require.Equalf(t, "global-value", got, "%s must fall back to the global row", key)
		require.Equal(t, "global", scope)
	}
}

// TestUnit_SetString_RefusesWorkspaceScopedKeys pins the sealed write door: a
// workspace-scoped key cannot be written through the global-only helper, so a
// writer cannot silently land in a row no workspace-aware reader consults.
func TestUnit_SetString_RefusesWorkspaceScopedKeys(t *testing.T) {
	ctx, store := scopeStore(t)
	for _, key := range clikv.WorkspaceScopedKeys() {
		require.ErrorContainsf(t, clikv.SetString(ctx, store, key, "x"), "workspace-scoped",
			"SetString must refuse %q and name WriteConfig", key)
		require.Empty(t, clikv.Read(ctx, store, key), "the refused write must persist nothing")
	}
	require.NoError(t, clikv.SetString(ctx, store, "default-model", "gpt-5-mini"),
		"a global key still writes through SetString")
	require.Equal(t, "gpt-5-mini", clikv.Read(ctx, store, "default-model"))
}

// TestUnit_SetHITLPolicy_LandsWhereConfigSetWrites pins that ACP's /policy
// and `contenox config set hitl-policy-name` write the same row: the two
// doors that diverged, one global and one workspace-scoped.
func TestUnit_SetHITLPolicy_LandsWhereConfigSetWrites(t *testing.T) {
	ctx, store := scopeStore(t)

	require.NoError(t, clikv.SetHITLPolicy(ctx, store, "ws-a", "hitl-policy-strict.json"))
	got, _ := clikv.ReadConfig(ctx, store, "ws-a", clikv.KeyHITLPolicyName)
	require.Equal(t, "hitl-policy-strict.json", got, "/policy must be visible to `config get`")

	require.NoError(t, clikv.WriteConfig(ctx, store, "ws-a", clikv.KeyHITLPolicyName, "hitl-policy-dev.json"))
	require.Equal(t, "hitl-policy-dev.json", clikv.ReadHITLPolicy(ctx, store, "ws-a"),
		"`config set` must be visible to /policy's own reader")
}

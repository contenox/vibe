package hitlservice_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/contenox/contenox/internal/services/clikv"
	"github.com/contenox/contenox/internal/services/hitlservice"
	"github.com/contenox/contenox/internal/store/runtimetypes"
	libdb "github.com/contenox/contenox/libdbexec"
	"github.com/contenox/contenox/libtracker"
	"github.com/stretchr/testify/require"
)

func policyScopeFixture(t *testing.T, workspaceID string) (context.Context, hitlservice.Service, runtimetypes.Store) {
	t.Helper()
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "allow.json"),
		[]byte(`{"default_action":"allow","rules":[]}`), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "deny.json"),
		[]byte(`{"default_action":"deny","rules":[]}`), 0o644))

	ctx := context.Background()
	db, err := libdb.NewSQLiteDBManager(ctx, filepath.Join(t.TempDir(), "policy-scope.db"), runtimetypes.SchemaSQLite)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	store := runtimetypes.New(db.WithoutTransaction())

	svc := hitlservice.NewWithDefaultPolicy(hitlservice.NewFSPolicySource(dir),
		runtimetypes.LocalTenantID, store, libtracker.NoopTracker{}, "allow.json")
	hitlservice.SetWorkspaceID(svc, workspaceID)
	return ctx, svc, store
}

// TestUnit_Evaluate_ReadsThePolicyTheCLIWrote pins that a write through the
// config-set path is read by Evaluate at the same scope.
func TestUnit_Evaluate_ReadsThePolicyTheCLIWrote(t *testing.T) {
	const workspaceID = "ws-anti-regression"
	ctx, svc, store := policyScopeFixture(t, workspaceID)

	result, err := svc.Evaluate(ctx, "local_fs", "write_file", nil)
	require.NoError(t, err)
	require.Equal(t, hitlservice.ActionAllow, result.Action)
	require.Equal(t, "allow.json", result.PolicyName)

	require.NoError(t, clikv.WriteConfig(ctx, store, workspaceID, clikv.KeyHITLPolicyName, "deny.json"),
		"the exact call `contenox config set hitl-policy-name` makes")

	result, err = svc.Evaluate(ctx, "local_fs", "write_file", nil)
	require.NoError(t, err)
	require.Equal(t, "deny.json", result.PolicyName,
		"the evaluator must read the row the CLI wrote, not a differently-scoped one")
	require.Equal(t, hitlservice.ActionDeny, result.Action)

	got, scope := clikv.ReadConfig(ctx, store, workspaceID, clikv.KeyHITLPolicyName)
	require.Equal(t, "deny.json", got)
	require.Equal(t, "workspace", scope)
}

// TestUnit_Evaluate_FallsBackToTheGlobalRow pins that a workspace with no
// policy override still falls back to the global row.
func TestUnit_Evaluate_FallsBackToTheGlobalRow(t *testing.T) {
	ctx, svc, store := policyScopeFixture(t, "ws-without-override")

	require.NoError(t, clikv.WriteConfig(ctx, store, "", clikv.KeyHITLPolicyName, "deny.json"))

	result, err := svc.Evaluate(ctx, "local_fs", "write_file", nil)
	require.NoError(t, err)
	require.Equal(t, "deny.json", result.PolicyName)
	require.Equal(t, hitlservice.ActionDeny, result.Action)
}

// TestUnit_Evaluate_WorkspaceRowWinsOverTheGlobalRow pins that a workspace's
// own policy wins over the global row.
func TestUnit_Evaluate_WorkspaceRowWinsOverTheGlobalRow(t *testing.T) {
	const workspaceID = "ws-strict-project"
	ctx, svc, store := policyScopeFixture(t, workspaceID)

	require.NoError(t, clikv.WriteConfig(ctx, store, "", clikv.KeyHITLPolicyName, "allow.json"))
	require.NoError(t, clikv.WriteConfig(ctx, store, workspaceID, clikv.KeyHITLPolicyName, "deny.json"))

	result, err := svc.Evaluate(ctx, "local_fs", "write_file", nil)
	require.NoError(t, err)
	require.Equal(t, "deny.json", result.PolicyName)
	require.Equal(t, hitlservice.ActionDeny, result.Action)
}

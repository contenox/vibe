// policy_scope_test.go pins the writer/reader scope agreement for the active
// HITL policy: what `contenox config set hitl-policy-name` writes is what
// Evaluate gates on. The bug it guards against was silent — the CLI wrote a
// workspace row, the evaluator read the global one, and switching policies
// did nothing.
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

// policyScopeFixture writes an allow-policy and a deny-policy where the FS
// source finds them, and returns a service over a real KV store.
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

// TestUnit_Evaluate_ReadsThePolicyTheCLIWrote is the anti-regression test for
// the scope split: write through the `contenox config set` path
// (clikv.WriteConfig at the resolved workspace), read through the evaluator,
// and assert they see one value. A reader re-scoped to the global row alone
// fails here.
func TestUnit_Evaluate_ReadsThePolicyTheCLIWrote(t *testing.T) {
	const workspaceID = "ws-anti-regression"
	ctx, svc, store := policyScopeFixture(t, workspaceID)

	// Baseline: the fallback policy allows, so a change of verdict below can
	// only come from the KV the CLI wrote.
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

	// The same value through the reader the CLI's `config get` uses.
	got, scope := clikv.ReadConfig(ctx, store, workspaceID, clikv.KeyHITLPolicyName)
	require.Equal(t, "deny.json", got)
	require.Equal(t, "workspace", scope)
}

// TestUnit_Evaluate_FallsBackToTheGlobalRow pins the inheritance leg: a
// workspace that never set a policy of its own still sees an operator's
// global default, so the fix adds a scope rather than hiding the old row.
func TestUnit_Evaluate_FallsBackToTheGlobalRow(t *testing.T) {
	ctx, svc, store := policyScopeFixture(t, "ws-without-override")

	require.NoError(t, clikv.WriteConfig(ctx, store, "", clikv.KeyHITLPolicyName, "deny.json"))

	result, err := svc.Evaluate(ctx, "local_fs", "write_file", nil)
	require.NoError(t, err)
	require.Equal(t, "deny.json", result.PolicyName)
	require.Equal(t, hitlservice.ActionDeny, result.Action)
}

// TestUnit_Evaluate_WorkspaceRowWinsOverTheGlobalRow pins the precedence one
// operator with two projects depends on: a project's own policy is not
// overridden by whatever the last global write happened to be.
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

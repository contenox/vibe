// hitl_workspace_scope_test.go pins the CLI end of the writer/reader scope
// agreement: the service every `contenox` command gates on must resolve
// cli.hitl-policy-name at the SAME workspace `contenox config set` writes it.
package contenoxcli

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/contenox/contenox/internal/services/clikv"
	"github.com/contenox/contenox/internal/services/hitlservice"
	"github.com/contenox/contenox/internal/services/project"
	"github.com/contenox/contenox/internal/store/runtimetypes"
	libdb "github.com/contenox/contenox/libdbexec"
	"github.com/contenox/contenox/libtracker"
	"github.com/stretchr/testify/require"
)

// TestUnit_NewHITLService_EvaluatesThePolicyConfigSetWrote drives the exact
// pair an operator does: `contenox config set hitl-policy-name deny.json` in
// a project, then a gated tool call in that same project.
func TestUnit_NewHITLService_EvaluatesThePolicyConfigSetWrote(t *testing.T) {
	ctx := context.Background()
	contenoxDir := t.TempDir()
	marker, err := project.EnsureInContenoxDir(contenoxDir, "scope-project")
	require.NoError(t, err)
	require.NotEmpty(t, marker.ID)
	require.Equal(t, marker.ID, ResolveWorkspaceID(contenoxDir),
		"the workspace the CLI writes config under")

	require.NoError(t, os.WriteFile(filepath.Join(contenoxDir, "deny.json"),
		[]byte(`{"default_action":"deny","rules":[]}`), 0o644))

	db, err := libdb.NewSQLiteDBManager(ctx, filepath.Join(t.TempDir(), "hitl-scope.db"), runtimetypes.SchemaSQLite)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	store := runtimetypes.New(db.WithoutTransaction())

	svc := newHITLService(context.Background(), contenoxDir, store, libtracker.NoopTracker{}, "")

	// What `contenox config set hitl-policy-name deny.json` persists.
	require.NoError(t, clikv.WriteConfig(ctx, store, ResolveWorkspaceID(contenoxDir),
		clikv.KeyHITLPolicyName, "deny.json"))

	result, err := svc.Evaluate(ctx, "local_fs", "write_file", nil)
	require.NoError(t, err)
	require.Equal(t, "deny.json", result.PolicyName,
		"the CLI's evaluator must be bound to the workspace `config set` wrote")
	require.Equal(t, hitlservice.ActionDeny, result.Action)
}

// TestUnit_NewHITLService_UnmarkedDirUsesTheDefaultWorkspace pins the
// no-project case: ResolveWorkspaceID's default id is a real workspace, so
// writer and reader still agree rather than falling apart into two rows.
func TestUnit_NewHITLService_UnmarkedDirUsesTheDefaultWorkspace(t *testing.T) {
	ctx := context.Background()
	contenoxDir := t.TempDir()
	require.Equal(t, DefaultWorkspaceID, ResolveWorkspaceID(contenoxDir))

	require.NoError(t, os.WriteFile(filepath.Join(contenoxDir, "deny.json"),
		[]byte(`{"default_action":"deny","rules":[]}`), 0o644))

	db, err := libdb.NewSQLiteDBManager(ctx, filepath.Join(t.TempDir(), "hitl-scope-default.db"), runtimetypes.SchemaSQLite)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	store := runtimetypes.New(db.WithoutTransaction())

	svc := newHITLService(context.Background(), contenoxDir, store, libtracker.NoopTracker{}, "")
	require.NoError(t, clikv.WriteConfig(ctx, store, ResolveWorkspaceID(contenoxDir),
		clikv.KeyHITLPolicyName, "deny.json"))

	result, err := svc.Evaluate(ctx, "local_fs", "write_file", nil)
	require.NoError(t, err)
	require.Equal(t, "deny.json", result.PolicyName)
	require.Equal(t, hitlservice.ActionDeny, result.Action)
}

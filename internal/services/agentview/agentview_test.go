package agentview_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/contenox/contenox/internal/services/agentview"
	"github.com/contenox/contenox/internal/services/hitlservice"
	"github.com/contenox/contenox/internal/services/vfs"
	"github.com/contenox/contenox/libtracker"
	"github.com/stretchr/testify/require"
)

// nopKV is a KVReader whose lookups always miss, forcing the constructor fallback policy.
type nopKV struct{}

func (nopKV) GetKV(context.Context, string, interface{}) error { return os.ErrNotExist }

const testTenant = "tenant-agentview"

// seededEvaluator returns an Evaluator bound to root's view and a hitlservice pinned to policyJSON.
func seededEvaluator(t *testing.T, root, policyName, policyJSON string) *agentview.Evaluator {
	t.Helper()
	policyDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(policyDir, policyName), []byte(policyJSON), 0o644))
	svc := hitlservice.NewWithDefaultPolicy(
		hitlservice.NewFSPolicySource(policyDir), testTenant, nopKV{}, libtracker.NoopTracker{}, policyName)
	view, err := vfs.OpenView(root)
	require.NoError(t, err)
	return agentview.NewEvaluator(view, svc, policyName)
}

// denySecretPolicy denies reads/writes under secret/**, marks staged/** approve-on-read.
const denySecretPolicy = `{
  "default_action": "approve",
  "rules": [
    { "tools": "local_fs", "tool": "read_file",  "action": "deny",    "when": [{ "key": "path", "op": "glob", "value": "secret/**" }] },
    { "tools": "local_fs", "tool": "write_file", "action": "deny",    "when": [{ "key": "path", "op": "glob", "value": "secret/**" }] },
    { "tools": "local_fs", "tool": "read_file",  "action": "approve", "when": [{ "key": "path", "op": "glob", "value": "staged/**" }] },
    { "tools": "local_fs", "tool": "read_file",  "action": "allow" },
    { "tools": "local_fs", "tool": "list_dir",   "action": "allow" },
    { "tools": "local_fs", "tool": "write_file", "action": "approve" }
  ]
}`

func TestVerdict_DenyGlobIsTruthful(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "secret"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(root, "secret", "x.txt"), []byte("s"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(root, "main.go"), []byte("m"), 0o644))

	ev := seededEvaluator(t, root, "hitl-policy-test.json", denySecretPolicy)
	ctx := context.Background()

	secret := ev.Verdict(ctx, "secret/x.txt", false)
	require.True(t, secret.Reachable)
	require.Equal(t, hitlservice.ActionDeny, secret.Read)
	require.Equal(t, hitlservice.ActionDeny, secret.Write)
	require.NotEmpty(t, secret.ReadReason, "a deny verdict must carry a reason")
	require.NotEmpty(t, secret.WriteReason)

	main := ev.Verdict(ctx, "main.go", false)
	require.True(t, main.Reachable)
	require.Equal(t, hitlservice.ActionAllow, main.Read)
	require.Empty(t, main.ReadReason, "an allow verdict must not add reason noise")
	require.Equal(t, hitlservice.ActionApprove, main.Write)
	require.NotEmpty(t, main.WriteReason)
}

func TestVerdict_ApproveGlob(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "staged"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(root, "staged", "draft.md"), []byte("d"), 0o644))

	ev := seededEvaluator(t, root, "hitl-policy-test.json", denySecretPolicy)
	v := ev.Verdict(context.Background(), "staged/draft.md", false)
	require.True(t, v.Reachable)
	require.Equal(t, hitlservice.ActionApprove, v.Read)
	require.NotEmpty(t, v.ReadReason)
}

func TestVerdict_DirectoryUsesListDir(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "secret"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(root, "src"), 0o755))

	ev := seededEvaluator(t, root, "hitl-policy-test.json", denySecretPolicy)
	ctx := context.Background()

	src := ev.Verdict(ctx, "src", true)
	require.True(t, src.Reachable)
	require.Equal(t, hitlservice.ActionAllow, src.Read)
	require.Equal(t, hitlservice.ActionApprove, src.Write)

	secret := ev.Verdict(ctx, "secret", true)
	require.Equal(t, hitlservice.ActionDeny, secret.Write)
}

func TestVerdict_UnreachableSymlinkEscape(t *testing.T) {
	outside := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(outside, "target.txt"), []byte("x"), 0o644))
	root := t.TempDir()
	require.NoError(t, os.Symlink(filepath.Join(outside, "target.txt"), filepath.Join(root, "escape.txt")))

	ev := seededEvaluator(t, root, "hitl-policy-test.json", denySecretPolicy)
	v := ev.Verdict(context.Background(), "escape.txt", false)
	require.False(t, v.Reachable)
	require.Empty(t, string(v.Read))
	require.Empty(t, string(v.Write))
	require.Empty(t, v.ReadReason)
	require.Empty(t, v.WriteReason)
}

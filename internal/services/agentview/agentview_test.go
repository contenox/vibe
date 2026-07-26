package agentview_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/contenox/beam/internal/libtracker"
	"github.com/contenox/beam/internal/services/agentview"
	"github.com/contenox/beam/internal/services/hitlservice"
	"github.com/contenox/beam/internal/services/vfs"
	"github.com/stretchr/testify/require"
)

// nopKV is a KVReader whose lookups always miss, so the hitlservice uses its
// constructor fallback policy (the seeded test policy) rather than any active
// policy KV key.
type nopKV struct{}

func (nopKV) GetKV(context.Context, string, interface{}) error { return os.ErrNotExist }

const testTenant = "tenant-agentview"

// seededEvaluator writes policyJSON as policyName into a fresh policy dir and
// returns an Evaluator bound to root's view + a hitlservice pinned to that
// policy.
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

// denySecretPolicy denies reads/writes under secret/**, allows read_file and
// list_dir elsewhere, requires approval for write_file elsewhere, and marks a
// specific staged/** path as approve-on-read.
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

	// A path matching the deny glob: read AND write are denied.
	secret := ev.Verdict(ctx, "secret/x.txt", false)
	require.True(t, secret.Reachable)
	require.Equal(t, hitlservice.ActionDeny, secret.Read)
	require.Equal(t, hitlservice.ActionDeny, secret.Write)
	require.NotEmpty(t, secret.ReadReason, "a deny verdict must carry a reason")
	require.NotEmpty(t, secret.WriteReason)

	// A non-matching path: read allowed (no reason noise), write requires approval.
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

	// list_dir on a normal dir is allowed; write (create-inside) requires approval.
	src := ev.Verdict(ctx, "src", true)
	require.True(t, src.Reachable)
	require.Equal(t, hitlservice.ActionAllow, src.Read)
	require.Equal(t, hitlservice.ActionApprove, src.Write)

	// The secret dir itself is denied for write (create-inside) by secret/**,
	// which matches the directory node too.
	secret := ev.Verdict(ctx, "secret", true)
	require.Equal(t, hitlservice.ActionDeny, secret.Write)
}

func TestVerdict_UnreachableSymlinkEscape(t *testing.T) {
	outside := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(outside, "target.txt"), []byte("x"), 0o644))
	root := t.TempDir()
	// A symlink inside the root pointing outside it: reachable must be false and
	// no policy actions are populated (the boundary is the answer).
	require.NoError(t, os.Symlink(filepath.Join(outside, "target.txt"), filepath.Join(root, "escape.txt")))

	ev := seededEvaluator(t, root, "hitl-policy-test.json", denySecretPolicy)
	v := ev.Verdict(context.Background(), "escape.txt", false)
	require.False(t, v.Reachable)
	require.Empty(t, string(v.Read))
	require.Empty(t, string(v.Write))
	require.Empty(t, v.ReadReason)
	require.Empty(t, v.WriteReason)
}

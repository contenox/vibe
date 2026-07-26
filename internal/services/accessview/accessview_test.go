package accessview_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/contenox/beam/internal/libtracker"
	"github.com/contenox/beam/internal/services/accessview"
	"github.com/contenox/beam/internal/services/hitlservice"
	"github.com/contenox/beam/internal/services/vfs"
	"github.com/stretchr/testify/require"
)

// nopKV is a KVReader whose lookups always miss, so the hitlservice under test
// uses its constructor fallback policy (the seeded test policy) rather than any
// active-policy KV key.
type nopKV struct{}

func (nopKV) GetKV(context.Context, string, interface{}) error { return os.ErrNotExist }

const testTenant = "tenant-accessview"

// seededEvaluator writes policyJSON as policyName into a fresh policy dir and
// returns an Evaluator bound to root's view + a hitlservice pinned to that
// policy, plus the policy's file name (for asserting EvaluateResponse.PolicyName).
func seededEvaluator(t *testing.T, root, policyName, policyJSON string) *accessview.Evaluator {
	t.Helper()
	policyDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(policyDir, policyName), []byte(policyJSON), 0o644))
	svc := hitlservice.NewWithDefaultPolicy(
		hitlservice.NewFSPolicySource(policyDir), testTenant, nopKV{}, libtracker.NoopTracker{}, policyName)
	view, err := vfs.OpenView(root)
	require.NoError(t, err)
	return accessview.NewEvaluator(view, svc)
}

// secretDenyPolicy denies read/write under .ssh/** with an explicit rule (so a
// matched-rule index is observable), allows read_file/list_dir elsewhere, and
// requires approval for write_file elsewhere (the default action, exercised via
// no matching rule).
const secretDenyPolicy = `{
  "default_action": "approve",
  "rules": [
    { "tools": "local_fs", "tool": "read_file",  "action": "deny", "when": [{ "key": "path", "op": "glob", "value": ".ssh/**" }] },
    { "tools": "local_fs", "tool": "write_file", "action": "deny", "when": [{ "key": "path", "op": "glob", "value": ".ssh/**" }] },
    { "tools": "local_fs", "tool": "read_file",  "action": "allow" },
    { "tools": "local_fs", "tool": "list_dir",   "action": "allow" }
  ]
}`

func TestEvaluate_ReachableFile_AllowReadApproveWriteDefault(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "main.go"), []byte("package main"), 0o644))

	ev := seededEvaluator(t, root, "hitl-policy-test.json", secretDenyPolicy)
	policyName, verdicts := ev.Evaluate(context.Background(), []string{"main.go"})

	require.Equal(t, "hitl-policy-test.json", policyName)
	require.Len(t, verdicts, 1)
	v := verdicts[0]
	require.Equal(t, "main.go", v.Path)
	require.True(t, v.Reachable)

	require.NotNil(t, v.Read)
	require.Equal(t, string(hitlservice.ActionAllow), v.Read.Action)
	require.Equal(t, hitlservice.ReasonMatchedRule, v.Read.Reason)
	require.NotNil(t, v.Read.Rule)

	require.NotNil(t, v.Write)
	require.Equal(t, string(hitlservice.ActionApprove), v.Write.Action)
	require.Equal(t, hitlservice.ReasonDefaultAction, v.Write.Reason)
	require.Nil(t, v.Write.Rule, "default_action must not carry a rule index")
}

func TestEvaluate_SecretPath_DenyWithMatchedRule(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, ".ssh"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(root, ".ssh", "id_rsa"), []byte("secret"), 0o600))

	ev := seededEvaluator(t, root, "hitl-policy-test.json", secretDenyPolicy)
	_, verdicts := ev.Evaluate(context.Background(), []string{".ssh/id_rsa"})

	require.Len(t, verdicts, 1)
	v := verdicts[0]
	require.True(t, v.Reachable)

	require.NotNil(t, v.Read)
	require.Equal(t, string(hitlservice.ActionDeny), v.Read.Action)
	require.Equal(t, hitlservice.ReasonMatchedRule, v.Read.Reason)
	require.NotNil(t, v.Read.Rule, "a deny-by-glob verdict must carry the matched rule index")

	require.NotNil(t, v.Write)
	require.Equal(t, string(hitlservice.ActionDeny), v.Write.Action)
	require.Equal(t, hitlservice.ReasonMatchedRule, v.Write.Reason)
	require.NotNil(t, v.Write.Rule)
}

func TestEvaluate_EscapingPath_UnreachableNilDimensions(t *testing.T) {
	outside := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(outside, "target.txt"), []byte("x"), 0o644))
	root := t.TempDir()
	require.NoError(t, os.Symlink(filepath.Join(outside, "target.txt"), filepath.Join(root, "escape.txt")))

	ev := seededEvaluator(t, root, "hitl-policy-test.json", secretDenyPolicy)
	_, verdicts := ev.Evaluate(context.Background(), []string{"escape.txt", "../../etc/passwd"})

	require.Len(t, verdicts, 2)
	for _, v := range verdicts {
		require.False(t, v.Reachable, "path %q must be reported unreachable", v.Path)
		require.Nil(t, v.Read)
		require.Nil(t, v.Write)
	}
}

func TestEvaluate_Directory_ReadUsesListDir(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "src"), 0o755))

	ev := seededEvaluator(t, root, "hitl-policy-test.json", secretDenyPolicy)
	_, verdicts := ev.Evaluate(context.Background(), []string{"src"})

	require.Len(t, verdicts, 1)
	v := verdicts[0]
	require.True(t, v.Reachable)
	require.NotNil(t, v.Read)
	// list_dir is allowed unconditionally by the seeded policy; read_file's
	// deny-glob for .ssh/** does not apply to "src", and there is no
	// unconditional read_file allow rule that would also match, so the ONLY
	// way this comes back "allow" is if list_dir (not read_file) was queried.
	require.Equal(t, string(hitlservice.ActionAllow), v.Read.Action)
}

func TestEvaluate_NonExistentPath_TreatedAsFile(t *testing.T) {
	root := t.TempDir()
	ev := seededEvaluator(t, root, "hitl-policy-test.json", secretDenyPolicy)
	_, verdicts := ev.Evaluate(context.Background(), []string{"does/not/exist.txt"})

	require.Len(t, verdicts, 1)
	v := verdicts[0]
	// The path resolves within root (vfs.Contain tolerates a missing leaf) but
	// does not exist on disk: reachable, treated as a file (read_file, which the
	// seeded policy allows unconditionally).
	require.True(t, v.Reachable)
	require.NotNil(t, v.Read)
	require.Equal(t, string(hitlservice.ActionAllow), v.Read.Action)
}

func TestEvaluate_EmptyBatch_StillNamesPolicy(t *testing.T) {
	root := t.TempDir()
	ev := seededEvaluator(t, root, "hitl-policy-test.json", secretDenyPolicy)

	policyName, verdicts := ev.Evaluate(context.Background(), nil)
	require.Equal(t, "hitl-policy-test.json", policyName)
	require.Empty(t, verdicts)
}

func TestEvaluate_AllUnreachableBatch_StillNamesPolicy(t *testing.T) {
	root := t.TempDir()
	ev := seededEvaluator(t, root, "hitl-policy-test.json", secretDenyPolicy)

	policyName, verdicts := ev.Evaluate(context.Background(), []string{"../escape"})
	require.Equal(t, "hitl-policy-test.json", policyName)
	require.Len(t, verdicts, 1)
	require.False(t, verdicts[0].Reachable)
}

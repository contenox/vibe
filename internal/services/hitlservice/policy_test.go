package hitlservice_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/contenox/contenox/internal/services/hitlservice"
	"github.com/contenox/contenox/libtracker"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testTenant = "00000000-0000-0000-0000-000000000001"

type nopKVReader struct{}

func (nopKVReader) GetKV(_ context.Context, _ string, _ any) error {
	return errors.New("not found")
}

type fixedKVReader struct{ name string }

func (f fixedKVReader) GetKV(_ context.Context, _ string, out any) error {
	if p, ok := out.(*string); ok {
		*p = f.name
	}
	return nil
}

func writePolicy(t *testing.T, dir, name string, data []byte) {
	t.Helper()
	require.NoError(t, os.WriteFile(filepath.Join(dir, name), data, 0o644))
}

func TestUnit_Evaluate_FallsBackToDefaultWhenFileMissing(t *testing.T) {
	t.Parallel()
	src := hitlservice.NewFSPolicySource(t.TempDir())
	svc := hitlservice.New(src, testTenant, nopKVReader{}, libtracker.NoopTracker{})
	result, err := svc.Evaluate(context.Background(), "local_fs", "write_file", nil)
	require.NoError(t, err)
	assert.Equal(t, hitlservice.ActionApprove, result.Action)
}

func TestUnit_Evaluate_LoadsFromSource(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	src := hitlservice.NewFSPolicySource(dir)
	ctx := context.Background()
	writePolicy(t, dir, "hitl-policy.json",
		[]byte(`{"default_action":"deny","rules":[{"tools":"webtools","tool":"call","action":"allow"}]}`))

	svc := hitlservice.New(src, testTenant, fixedKVReader{"hitl-policy.json"}, libtracker.NoopTracker{})
	result, err := svc.Evaluate(ctx, "webtools", "call", nil)
	require.NoError(t, err)
	assert.Equal(t, hitlservice.ActionAllow, result.Action)

	result, err = svc.Evaluate(ctx, "local_fs", "write_file", nil)
	require.NoError(t, err)
	assert.Equal(t, hitlservice.ActionDeny, result.Action)
}

func TestUnit_Evaluate_WhenConditionFromSource(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	src := hitlservice.NewFSPolicySource(dir)
	ctx := context.Background()
	writePolicy(t, dir, "hitl-policy.json", []byte(`{
		"default_action": "approve",
		"rules": [
			{"tools":"local_fs","tool":"write_file","when":[{"key":"path","op":"glob","value":"./src/**"}],"action":"allow"},
			{"tools":"local_fs","tool":"write_file","action":"approve","timeout_s":30,"on_timeout":"deny"}
		]
	}`))

	svc := hitlservice.New(src, testTenant, fixedKVReader{"hitl-policy.json"}, libtracker.NoopTracker{})

	result, err := svc.Evaluate(ctx, "local_fs", "write_file", map[string]any{"path": "./src/main.go"})
	require.NoError(t, err)
	assert.Equal(t, hitlservice.ActionAllow, result.Action)
	assert.Equal(t, 0, result.TimeoutS)

	result, err = svc.Evaluate(ctx, "local_fs", "write_file", map[string]any{"path": "./etc/passwd"})
	require.NoError(t, err)
	assert.Equal(t, hitlservice.ActionApprove, result.Action)
	assert.Equal(t, 30, result.TimeoutS)
	assert.Equal(t, hitlservice.ActionDeny, result.OnTimeout)
}

func TestUnit_Evaluate_HostConditionDeniesByURLHost(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	src := hitlservice.NewFSPolicySource(dir)
	ctx := context.Background()
	writePolicy(t, dir, "hitl-policy.json", []byte(`{
		"default_action": "allow",
		"rules": [
			{"tools":"webtools","tool":"*","action":"deny","when":[{"key":"url","op":"host","value":"localhost,127.0.0.1,169.254.169.254,metadata.google.internal"}]}
		]
	}`))
	svc := hitlservice.New(src, testTenant, fixedKVReader{"hitl-policy.json"}, libtracker.NoopTracker{})

	// Host parsing must survive scheme, :port and path.
	denied := []string{
		"http://localhost/",
		"http://localhost:8080/api",
		"https://127.0.0.1/x",
		"http://169.254.169.254:80/latest/meta-data",
		"http://metadata.google.internal/computeMetadata/v1/",
		"http://sub.metadata.google.internal/",
	}
	for _, u := range denied {
		r, err := svc.Evaluate(ctx, "webtools", "web_get", map[string]any{"url": u})
		require.NoError(t, err)
		assert.Equal(t, hitlservice.ActionDeny, r.Action, "host deny must block %q", u)
	}

	// A host that only contains the pattern as a substring must NOT be denied.
	allowed := []string{
		"http://example.com/?redir=localhost",
		"https://api.example.com/v1",
	}
	for _, u := range allowed {
		r, err := svc.Evaluate(ctx, "webtools", "web_get", map[string]any{"url": u})
		require.NoError(t, err)
		assert.Equal(t, hitlservice.ActionAllow, r.Action, "non-matching host must pass %q", u)
	}
}

func TestUnit_Evaluate_ResolvesFromKV(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	src := hitlservice.NewFSPolicySource(dir)
	ctx := context.Background()
	writePolicy(t, dir, "hitl-policy-strict.json", []byte(`{"default_action":"deny","rules":[]}`))

	svc := hitlservice.New(src, testTenant, fixedKVReader{"hitl-policy-strict.json"}, libtracker.NoopTracker{})
	result, err := svc.Evaluate(ctx, "local_fs", "write_file", nil)
	require.NoError(t, err)
	assert.Equal(t, hitlservice.ActionDeny, result.Action, "strict policy (deny-by-default) should deny write_file")
}

func TestUnit_Evaluate_FallsBackToBuiltinWhenKVEmptyAndFileMissing(t *testing.T) {
	t.Parallel()
	src := hitlservice.NewFSPolicySource(t.TempDir())
	svc := hitlservice.New(src, testTenant, nopKVReader{}, libtracker.NoopTracker{})
	result, err := svc.Evaluate(context.Background(), "local_fs", "write_file", nil)
	require.NoError(t, err)
	assert.Equal(t, hitlservice.ActionApprove, result.Action, "built-in default requires approval for write_file")
}

func TestUnit_Evaluate_BuiltinDefaultIsFailClosedForUnaccountedTool(t *testing.T) {
	t.Parallel()
	src := hitlservice.NewFSPolicySource(t.TempDir())
	svc := hitlservice.New(src, testTenant, nopKVReader{}, libtracker.NoopTracker{})

	r, err := svc.Evaluate(context.Background(), "some_mcp_server", "arbitrary_tool", nil)
	require.NoError(t, err)
	assert.Equal(t, hitlservice.ActionApprove, r.Action, "a tool no policy rule accounts for must fail closed to approve, not silently allow")

	r, err = svc.Evaluate(context.Background(), "local_shell", "local_shell", nil)
	require.NoError(t, err)
	assert.Equal(t, hitlservice.ActionApprove, r.Action, "the editor shell must require approval; it is the model's preferred and most powerful tool")

	r, err = svc.Evaluate(context.Background(), "local_fs", "read_file", nil)
	require.NoError(t, err)
	assert.Equal(t, hitlservice.ActionApprove, r.Action, "even reads ask under the fallback: it is the absence of an envelope, and allow tiers exist only in seeded, readable policy files")
}

func TestUnit_Evaluate_DefaultPolicyOverrideSelectsACPPolicyWhenKVUnset(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	src := hitlservice.NewFSPolicySource(dir)
	ctx := context.Background()

	writePolicy(t, dir, "hitl-policy-acp.json",
		[]byte(`{"default_action":"approve","rules":[{"tools":"local_fs","tool":"read_file","action":"allow"},{"tools":"local_shell","tool":"local_shell","action":"approve"}]}`))
	writePolicy(t, dir, "hitl-policy.json", []byte(`{"default_action":"allow"}`))

	svc := hitlservice.NewWithDefaultPolicy(src, testTenant, nopKVReader{}, libtracker.NoopTracker{}, "hitl-policy-acp.json")
	r, err := svc.Evaluate(ctx, "local_shell", "local_shell", nil)
	require.NoError(t, err)
	assert.Equal(t, hitlservice.ActionApprove, r.Action, "with no KV set, the ACP entrypoint must fall back to hitl-policy-acp.json, not the generic default")
	r, err = svc.Evaluate(ctx, "local_fs", "read_file", nil)
	require.NoError(t, err)
	assert.Equal(t, hitlservice.ActionAllow, r.Action)

	explicit := hitlservice.NewWithDefaultPolicy(src, testTenant, fixedKVReader{"hitl-policy.json"}, libtracker.NoopTracker{}, "hitl-policy-acp.json")
	r, err = explicit.Evaluate(ctx, "local_shell", "local_shell", nil)
	require.NoError(t, err)
	assert.Equal(t, hitlservice.ActionAllow, r.Action, "an explicit hitl-policy-name KV must still override the per-process ACP default")
}

// TestUnit_PolicyVersion_AbsentIsCurrentAndLoadsUnchanged pins that a policy
// with no version field loads and evaluates identically to one declaring version 1.
func TestUnit_PolicyVersion_AbsentIsCurrentAndLoadsUnchanged(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	const rules = `"default_action":"approve","rules":[{"tools":"webtools","tool":"call","action":"allow"}]`

	dir := t.TempDir()
	writePolicy(t, dir, "absent.json", []byte(`{`+rules+`}`))
	writePolicy(t, dir, "explicit.json", []byte(`{"version":1,`+rules+`}`))
	src := hitlservice.NewFSPolicySource(dir)

	for _, name := range []string{"absent.json", "explicit.json"} {
		svc := hitlservice.New(src, testTenant, fixedKVReader{name}, libtracker.NoopTracker{})
		r, err := svc.Evaluate(ctx, "webtools", "call", nil)
		require.NoError(t, err)
		assert.Equalf(t, hitlservice.ActionAllow, r.Action, "%s must evaluate identically", name)
		r, err = svc.Evaluate(ctx, "local_fs", "write_file", nil)
		require.NoError(t, err)
		assert.Equalf(t, hitlservice.ActionApprove, r.Action, "%s must evaluate identically", name)
	}

	require.NoError(t, hitlservice.VetPolicy([]byte(`{`+rules+`}`)))
	require.NoError(t, hitlservice.VetPolicy([]byte(`{"version":1,`+rules+`}`)))
}

// TestUnit_PolicyVersion_UnloadableVersionIsRefused pins that a version this
// binary cannot load fails the policy to load, falling back to the
// fail-closed default.
func TestUnit_PolicyVersion_UnloadableVersionIsRefused(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dir := t.TempDir()
	src := hitlservice.NewFSPolicySource(dir)

	for name, doc := range map[string]string{
		"future.json":   `{"version":9999,"default_action":"allow"}`,
		"negative.json": `{"version":-1,"default_action":"allow"}`,
	} {
		writePolicy(t, dir, name, []byte(doc))

		err := hitlservice.VetPolicy([]byte(doc))
		require.Errorf(t, err, "%s must not vet", name)
		assert.ErrorIsf(t, err, hitlservice.ErrPolicyVersion, "%s", name)

		// The load path falls back to the built-in fail-closed policy: an unreadable version never leaves a run less gated.
		svc := hitlservice.New(src, testTenant, fixedKVReader{name}, libtracker.NoopTracker{})
		r, err := svc.Evaluate(ctx, "local_shell", "local_shell", nil)
		require.NoError(t, err)
		assert.Equalf(t, hitlservice.ActionApprove, r.Action,
			"%s declares a version this binary cannot load and must fall back to the fail-closed default, never to its own allow", name)
	}
}

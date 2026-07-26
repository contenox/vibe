package hitlservice_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/contenox/beam/internal/kernel/taskengine"
	"github.com/contenox/beam/internal/libtracker"
	"github.com/contenox/beam/internal/services/hitlservice"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type erroringSink struct{}

func (erroringSink) PublishTaskEvent(context.Context, taskengine.TaskEvent) error {
	return errors.New("sink down")
}
func (erroringSink) Enabled() bool { return true }

func seedAndService(t *testing.T, json string) hitlservice.Service {
	t.Helper()
	dir := t.TempDir()
	writePolicy(t, dir, "hitl-policy.json", []byte(json))
	return hitlservice.New(hitlservice.NewFSPolicySource(dir), testTenant, fixedKVReader{"hitl-policy.json"}, libtracker.NoopTracker{})
}

func TestUnit_Evaluate_CharClassGlobIsNotTreatedAsLiteral(t *testing.T) {
	t.Parallel()
	svc := seedAndService(t, `{"default_action":"approve","rules":[
		{"tools":"local_fs","tool":"read_file","when":[{"key":"path","op":"glob","value":"/[eE]tc/passwd"}],"action":"deny"}]}`)
	r, err := svc.Evaluate(context.Background(), "local_fs", "read_file", map[string]any{"path": "/etc/passwd"})
	require.NoError(t, err)
	assert.Equal(t, hitlservice.ActionDeny, r.Action, "a [class] glob must be matched, not compared literally")
}

func TestUnit_Evaluate_ArrayArgIsCheckedElementwise(t *testing.T) {
	t.Parallel()
	policy := `{"default_action":"approve","rules":[
		{"tools":"local_fs","tool":"*","when":[{"key":"path","op":"glob","value":"**/.ssh/**"}],"action":"deny"}]}`
	for _, arg := range []any{
		[]any{"notes.txt", "/home/u/.ssh/id_rsa"},
		[]string{"notes.txt", "/home/u/.ssh/id_rsa"},
	} {
		svc := seedAndService(t, policy)
		r, err := svc.Evaluate(context.Background(), "local_fs", "read_file", map[string]any{"path": arg})
		require.NoError(t, err)
		assert.Equal(t, hitlservice.ActionDeny, r.Action, "a secret hidden in an array element must still be denied (%T)", arg)
	}
}

func TestUnit_Evaluate_MalformedBraceGlobFailsClosed(t *testing.T) {
	t.Parallel()
	// Unbalanced brace: the policy must be rejected at load, so evaluation
	// falls back to the hardened built-in default rather than running with a
	// deny rule that path.Match would silently never fire.
	svc := seedAndService(t, `{"default_action":"allow","rules":[
		{"tools":"local_fs","tool":"*","when":[{"key":"path","op":"glob","value":"**/{.ssh,.gnupg/**"}],"action":"deny"}]}`)
	r, err := svc.Evaluate(context.Background(), "local_fs", "read_file", map[string]any{"path": "/home/u/.ssh/id_rsa"})
	require.NoError(t, err)
	assert.Equal(t, hitlservice.ActionDeny, r.Action, "rejected policy must fall back to the secret-denying default, not its own default_action:allow")
}

func TestUnit_RequestApproval_FailsFastWhenEventSinkErrors(t *testing.T) {
	t.Parallel()
	// A durable store is required (see durable_approval_test.go's
	// newDurableService): RequestApproval persists the pending row before it
	// ever reaches the publish call this test means to exercise, so a bare
	// KVReader fake (nopKVReader, used by this package's Evaluate()-only
	// tests) would fail one step earlier, on "durable approval store not
	// configured", never reaching erroringSink at all.
	_, store, _ := setupHITLDB(t)
	svc := newDurableService(t, store)

	done := make(chan error, 1)
	go func() {
		_, err := svc.RequestApproval(context.Background(), hitlservice.ApprovalRequest{
			ToolsName: "local_fs", ToolName: "write_file",
		}, erroringSink{})
		done <- err
	}()
	select {
	case err := <-done:
		require.Error(t, err, "must return the publish error instead of blocking forever")
	case <-time.After(2 * time.Second):
		t.Fatal("RequestApproval blocked after the event sink failed")
	}
}

func TestUnit_Evaluate_BraceGlobConditionMatchesAlternatives(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	ctx := context.Background()
	writePolicy(t, dir, "hitl-policy.json", []byte(`{
		"default_action": "approve",
		"rules": [
			{"tools":"local_fs","tool":"*","when":[{"key":"path","op":"glob","value":"**/{.ssh,.gnupg,.config/gcloud}/**"}],"action":"deny"},
			{"tools":"local_fs","tool":"read_file","action":"allow"}
		]
	}`))

	svc := hitlservice.New(hitlservice.NewFSPolicySource(dir), testTenant, fixedKVReader{"hitl-policy.json"}, libtracker.NoopTracker{})

	for _, secret := range []string{"/home/u/.ssh/id_rsa", "rel/.gnupg/secring", "/home/u/.config/gcloud/creds"} {
		r, err := svc.Evaluate(ctx, "local_fs", "read_file", map[string]any{"path": secret})
		require.NoError(t, err)
		assert.Equal(t, hitlservice.ActionDeny, r.Action, "brace alternative %q must be denied", secret)
	}

	r, err := svc.Evaluate(ctx, "local_fs", "read_file", map[string]any{"path": "/home/u/project/main.go"})
	require.NoError(t, err)
	assert.Equal(t, hitlservice.ActionAllow, r.Action, "a non-secret path must not match the brace pattern")
}

func TestUnit_Evaluate_BuiltinFallbackDeniesSecretReads(t *testing.T) {
	t.Parallel()
	svc := hitlservice.New(hitlservice.NewFSPolicySource(t.TempDir()), testTenant, nopKVReader{}, libtracker.NoopTracker{})
	ctx := context.Background()

	r, err := svc.Evaluate(ctx, "local_fs", "read_file", map[string]any{"path": "/home/u/.ssh/id_rsa"})
	require.NoError(t, err)
	assert.Equal(t, hitlservice.ActionDeny, r.Action, "built-in fallback must not allow reading SSH keys when the policy file is missing")

	r, err = svc.Evaluate(ctx, "local_fs", "read_file", map[string]any{"path": "src/main.go"})
	require.NoError(t, err)
	assert.Equal(t, hitlservice.ActionAllow, r.Action, "built-in fallback must still allow ordinary source reads")
}

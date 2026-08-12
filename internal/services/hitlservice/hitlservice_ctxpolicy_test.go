package hitlservice_test

import (
	"context"
	"sync"
	"testing"

	"github.com/contenox/contenox/internal/services/hitlservice"
	"github.com/contenox/contenox/libtracker"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	permissivePolicyName = "hitl-policy-permissive.json"
	strictPolicyName     = "hitl-policy-strict.json"
	permissivePolicyJSON = `{"default_action":"allow","rules":[{"tools":"local_fs","tool":"write_file","action":"allow"}]}`
	strictPolicyJSON     = `{"default_action":"deny","rules":[{"tools":"local_fs","tool":"write_file","action":"approve"}]}`
)

func twoPolicySource(t *testing.T) hitlservice.PolicySource {
	t.Helper()
	dir := t.TempDir()
	writePolicy(t, dir, permissivePolicyName, []byte(permissivePolicyJSON))
	writePolicy(t, dir, strictPolicyName, []byte(strictPolicyJSON))
	return hitlservice.NewFSPolicySource(dir)
}

// TestUnit_Evaluate_ContextPolicyOverridesGlobalKV pins that an explicit
// per-request policy wins over the process-global KV, and with no override
// Evaluate still reads the global KV.
func TestUnit_Evaluate_ContextPolicyOverridesGlobalKV(t *testing.T) {
	t.Parallel()
	src := twoPolicySource(t)
	svc := hitlservice.New(src, testTenant, fixedKVReader{strictPolicyName}, libtracker.NoopTracker{})

	base, err := svc.Evaluate(context.Background(), "local_fs", "write_file", nil)
	require.NoError(t, err)
	assert.Equal(t, hitlservice.ActionApprove, base.Action)
	assert.Equal(t, strictPolicyName, base.PolicyName)

	permCtx := hitlservice.WithPolicyName(context.Background(), permissivePolicyName)
	perm, err := svc.Evaluate(permCtx, "local_fs", "write_file", nil)
	require.NoError(t, err)
	assert.Equal(t, hitlservice.ActionAllow, perm.Action)
	assert.Equal(t, permissivePolicyName, perm.PolicyName)

	strictCtx := hitlservice.WithPolicyName(context.Background(), strictPolicyName)
	strict, err := svc.Evaluate(strictCtx, "local_fs", "write_file", nil)
	require.NoError(t, err)
	assert.Equal(t, hitlservice.ActionApprove, strict.Action)
	assert.Equal(t, strictPolicyName, strict.PolicyName)
}

// TestUnit_Evaluate_EmptyContextPolicyLeavesGlobalKVIntact pins that
// WithPolicyName with a blank name is a no-op: the service keeps reading the global KV.
func TestUnit_Evaluate_EmptyContextPolicyLeavesGlobalKVIntact(t *testing.T) {
	t.Parallel()
	src := twoPolicySource(t)
	svc := hitlservice.New(src, testTenant, fixedKVReader{permissivePolicyName}, libtracker.NoopTracker{})

	ctx := hitlservice.WithPolicyName(context.Background(), "   ") // whitespace -> no override
	res, err := svc.Evaluate(ctx, "local_fs", "write_file", nil)
	require.NoError(t, err)
	assert.Equal(t, hitlservice.ActionAllow, res.Action, "empty override must defer to the global KV (permissive)")
	assert.Equal(t, permissivePolicyName, res.PolicyName)
}

// TestUnit_Evaluate_ConcurrentSessionsGateIndependently pins that one shared
// service evaluating the same call under different per-request policies
// concurrently returns each caller its own verdict (run under -race).
func TestUnit_Evaluate_ConcurrentSessionsGateIndependently(t *testing.T) {
	t.Parallel()
	src := twoPolicySource(t)
	svc := hitlservice.New(src, testTenant, fixedKVReader{strictPolicyName}, libtracker.NoopTracker{})

	const iterations = 200
	var wg sync.WaitGroup
	errs := make(chan error, iterations*2)

	check := func(ctx context.Context, want hitlservice.Action, wantPolicy string) {
		defer wg.Done()
		res, err := svc.Evaluate(ctx, "local_fs", "write_file", nil)
		if err != nil {
			errs <- err
			return
		}
		if res.Action != want || res.PolicyName != wantPolicy {
			errs <- assertMismatch(want, wantPolicy, res)
		}
	}

	for i := 0; i < iterations; i++ {
		wg.Add(2)
		// Session A: permissive override -> allow.
		go check(hitlservice.WithPolicyName(context.Background(), permissivePolicyName), hitlservice.ActionAllow, permissivePolicyName)
		// Session B: no override -> global KV (strict) -> approve.
		go check(context.Background(), hitlservice.ActionApprove, strictPolicyName)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

func assertMismatch(wantAction hitlservice.Action, wantPolicy string, got hitlservice.EvaluationResult) error {
	return &mismatchError{wantAction: wantAction, wantPolicy: wantPolicy, got: got}
}

type mismatchError struct {
	wantAction hitlservice.Action
	wantPolicy string
	got        hitlservice.EvaluationResult
}

func (e *mismatchError) Error() string {
	return "policy leak: want action=" + string(e.wantAction) + " policy=" + e.wantPolicy +
		" got action=" + string(e.got.Action) + " policy=" + e.got.PolicyName
}

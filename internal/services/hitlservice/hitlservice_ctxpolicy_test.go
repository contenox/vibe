package hitlservice_test

import (
	"context"
	"sync"
	"testing"

	"github.com/contenox/beam/internal/libtracker"
	"github.com/contenox/beam/internal/services/hitlservice"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// permissive allows write_file outright; strict pauses it for approval. The two
// policies give write_file opposite verdicts so a test can prove which one a
// given Evaluate call resolved.
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

// TestUnit_Evaluate_ContextPolicyOverridesGlobalKV pins the load-bearing rule of
// the per-session HITL change: an explicit per-request policy (what an ACP prompt
// turn injects for its session) wins over the process-global cli.hitl-policy-name
// KV, and with NO override Evaluate still reads the global KV (CLI/stdio path is
// unchanged).
func TestUnit_Evaluate_ContextPolicyOverridesGlobalKV(t *testing.T) {
	t.Parallel()
	src := twoPolicySource(t)
	// The global KV points at the strict policy — the single-session fallback.
	svc := hitlservice.New(src, testTenant, fixedKVReader{strictPolicyName}, libtracker.NoopTracker{})

	// No override: falls through to the global KV (strict) -> write_file approves.
	base, err := svc.Evaluate(context.Background(), "local_fs", "write_file", nil)
	require.NoError(t, err)
	assert.Equal(t, hitlservice.ActionApprove, base.Action)
	assert.Equal(t, strictPolicyName, base.PolicyName)

	// Context override to the permissive policy wins over the global KV.
	permCtx := hitlservice.WithPolicyName(context.Background(), permissivePolicyName)
	perm, err := svc.Evaluate(permCtx, "local_fs", "write_file", nil)
	require.NoError(t, err)
	assert.Equal(t, hitlservice.ActionAllow, perm.Action)
	assert.Equal(t, permissivePolicyName, perm.PolicyName)

	// Context override to the strict policy is honored too (explicit).
	strictCtx := hitlservice.WithPolicyName(context.Background(), strictPolicyName)
	strict, err := svc.Evaluate(strictCtx, "local_fs", "write_file", nil)
	require.NoError(t, err)
	assert.Equal(t, hitlservice.ActionApprove, strict.Action)
	assert.Equal(t, strictPolicyName, strict.PolicyName)
}

// TestUnit_Evaluate_EmptyContextPolicyLeavesGlobalKVIntact proves WithPolicyName
// with an empty name is a no-op: a defaulting ACP session injects nothing and the
// shared service keeps reading the global KV.
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

// TestUnit_Evaluate_ConcurrentSessionsGateIndependently is the acp-shaped case:
// ONE shared hitlservice (as serve builds behind every ACP WebSocket session)
// evaluating the SAME tool call under different per-request policies concurrently
// must return each caller its own verdict. Run under -race to catch any shared
// mutable state that would let one session's policy leak into another's decision.
func TestUnit_Evaluate_ConcurrentSessionsGateIndependently(t *testing.T) {
	t.Parallel()
	src := twoPolicySource(t)
	// Global KV points at strict; the "session B" cohort uses NO override and so
	// resolves to it, while "session A" overrides to permissive per request.
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

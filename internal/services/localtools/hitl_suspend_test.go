package localtools_test

// Wrapper tests for the third HITL outcome (park-then-release). Timing is
// injected by shrinking the park window (SetParkWindow), a plain context
// deadline, so a short window exercises the production code path exactly.

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/contenox/contenox/internal/kernel/taskengine"
	"github.com/contenox/contenox/internal/services/hitlservice"
	"github.com/contenox/contenox/internal/services/localtools"
	"github.com/stretchr/testify/require"
)

// recordingApprovePolicy is a PolicyEvaluator that ALSO implements
// hitlservice.ApprovalRecorder — the capability that switches the wrapper
// onto the durable park path.
type recordingApprovePolicy struct {
	result hitlservice.EvaluationResult

	mu        sync.Mutex
	recorded  []string // approval IDs, in call order
	resolved  map[string]bool
	recordErr error
}

func (p *recordingApprovePolicy) Evaluate(context.Context, string, string, map[string]any) (hitlservice.EvaluationResult, error) {
	return p.result, nil
}

func (p *recordingApprovePolicy) RecordPendingApproval(_ context.Context, approvalID string, _ hitlservice.ApprovalRequest) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.recordErr != nil {
		return p.recordErr
	}
	p.recorded = append(p.recorded, approvalID)
	return nil
}

func (p *recordingApprovePolicy) ResolveApprovalInline(_ context.Context, approvalID string, approved bool) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.resolved == nil {
		p.resolved = map[string]bool{}
	}
	p.resolved[approvalID] = approved
	return nil
}

func newRecordingApprovePolicy() *recordingApprovePolicy {
	return &recordingApprovePolicy{result: hitlservice.EvaluationResult{Action: hitlservice.ActionApprove}}
}

var _ hitlservice.ApprovalRecorder = (*recordingApprovePolicy)(nil)

// blockUntilCtxDone is an ask that honors its context (like the ACP
// permission RPC): no verdict ever arrives, the park window decides.
func blockUntilCtxDone(ctx context.Context, _ hitlservice.ApprovalRequest) (bool, error) {
	<-ctx.Done()
	return false, ctx.Err()
}

func execGatedCall(t *testing.T, w *localtools.HITLWrapper, callID string) (any, taskengine.DataType, error) {
	t.Helper()
	ctx := context.Background()
	if callID != "" {
		ctx = context.WithValue(ctx, taskengine.ContextKeyToolCallID, callID)
	}
	return w.Exec(ctx, time.Now(), map[string]any{"path": "a.txt"}, false,
		&taskengine.ToolsCall{Name: "local_shell", ToolName: "run_command"})
}

// TestUnit_HITLWrapper_ParkThenRelease pins that no verdict within the park window yields the typed pending outcome, row recorded first and left pending.
func TestUnit_HITLWrapper_ParkThenRelease(t *testing.T) {
	inner := &mockInnerTools{}
	policy := newRecordingApprovePolicy()
	w := localtools.NewHITLWrapper(inner, blockUntilCtxDone, policy, nil)
	w.SetParkWindow(25 * time.Millisecond)

	start := time.Now()
	_, _, err := execGatedCall(t, w, "call-9")
	elapsed := time.Since(start)

	var pend *taskengine.ApprovalPendingError
	require.ErrorAs(t, err, &pend)
	require.Equal(t, "call-9", pend.ApprovalID, "approval ID == the engine-minted call ID")
	require.Equal(t, "run_command", pend.ToolName)
	require.Equal(t, []string{"call-9"}, policy.recorded, "the durable row precedes the release")
	require.Empty(t, policy.resolved, "the row stays pending — the human has not answered")
	require.Empty(t, inner.calls, "the gated tool must not run")
	require.GreaterOrEqual(t, elapsed, 25*time.Millisecond, "the fast-path park must actually wait its window")
}

// TestUnit_HITLWrapper_FastPathVerdictInsideWindow pins that an in-session verdict resolves normally and closes the durable row inline.
func TestUnit_HITLWrapper_FastPathVerdictInsideWindow(t *testing.T) {
	inner := &mockInnerTools{}
	policy := newRecordingApprovePolicy()
	w := localtools.NewHITLWrapper(inner, alwaysApprove, policy, nil)

	res, dt, err := execGatedCall(t, w, "call-1")
	require.NoError(t, err)
	require.Equal(t, "ok", res)
	require.Equal(t, taskengine.DataTypeString, dt)
	require.Equal(t, []string{"run_command"}, inner.calls)
	require.Equal(t, map[string]bool{"call-1": true}, policy.resolved)

	// Deny inside the window: the standard soft denial, row closed as denied.
	inner2 := &mockInnerTools{}
	policy2 := newRecordingApprovePolicy()
	w2 := localtools.NewHITLWrapper(inner2, alwaysDeny, policy2, nil)
	res2, _, err := execGatedCall(t, w2, "call-2")
	require.NoError(t, err)
	require.Equal(t, localtools.DenyMessage, res2)
	require.Empty(t, inner2.calls)
	require.Equal(t, map[string]bool{"call-2": false}, policy2.resolved)
}

// TestUnit_HITLWrapper_InjectedVerdict pins that a resumed run's pre-loaded verdict short-circuits the gate: no ask, no new row.
func TestUnit_HITLWrapper_InjectedVerdict(t *testing.T) {
	askCalled := false
	ask := func(ctx context.Context, req hitlservice.ApprovalRequest) (bool, error) {
		askCalled = true
		return false, nil
	}

	inner := &mockInnerTools{}
	policy := newRecordingApprovePolicy()
	w := localtools.NewHITLWrapper(inner, ask, policy, nil)

	ctx := context.WithValue(context.Background(), taskengine.ContextKeyToolCallID, "call-7")
	ctx = taskengine.WithApprovalVerdicts(ctx, map[string]bool{"call-7": true})
	res, _, err := w.Exec(ctx, time.Now(), map[string]any{}, false,
		&taskengine.ToolsCall{Name: "local_shell", ToolName: "run_command"})
	require.NoError(t, err)
	require.Equal(t, "ok", res)
	require.False(t, askCalled, "the human already answered this exact invocation")
	require.Empty(t, policy.recorded, "no second durable row for an answered call")

	// Denied verdict.
	ctx2 := context.WithValue(context.Background(), taskengine.ContextKeyToolCallID, "call-8")
	ctx2 = taskengine.WithApprovalVerdicts(ctx2, map[string]bool{"call-8": false})
	res2, _, err := w.Exec(ctx2, time.Now(), map[string]any{}, false,
		&taskengine.ToolsCall{Name: "local_shell", ToolName: "run_command"})
	require.NoError(t, err)
	require.Equal(t, localtools.DenyMessage, res2)
	require.False(t, askCalled)
	require.Len(t, inner.calls, 1, "only the approved call reached the inner repo")

	// A verdict for a DIFFERENT call must not leak onto this one: the gate
	// asks normally (and, blocking ask answering false, denies).
	ctx3 := context.WithValue(context.Background(), taskengine.ContextKeyToolCallID, "call-other")
	ctx3 = taskengine.WithApprovalVerdicts(ctx3, map[string]bool{"call-7": true})
	res3, _, err := w.Exec(ctx3, time.Now(), map[string]any{}, false,
		&taskengine.ToolsCall{Name: "local_shell", ToolName: "run_command"})
	require.NoError(t, err)
	require.Equal(t, localtools.DenyMessage, res3)
	require.True(t, askCalled)
}

// TestUnit_HITLWrapper_RuleTimeoutBeatsPark pins that a rule TimeoutS shorter than the park window resolves via OnTimeout instead of suspending.
func TestUnit_HITLWrapper_RuleTimeoutBeatsPark(t *testing.T) {
	inner := &mockInnerTools{}
	policy := newRecordingApprovePolicy()
	policy.result.TimeoutS = 1
	policy.result.OnTimeout = hitlservice.ActionDeny
	w := localtools.NewHITLWrapper(inner, blockUntilCtxDone, policy, nil)
	w.SetParkWindow(time.Hour) // rule bound must fire first

	res, dt, err := execGatedCall(t, w, "call-t")
	require.NoError(t, err, "a rule timeout resolves, never suspends")
	require.Equal(t, taskengine.DataTypeString, dt)
	require.Contains(t, res.(string), "timed out")
	require.Empty(t, inner.calls)
	require.Equal(t, map[string]bool{"call-t": false}, policy.resolved)
}

// TestUnit_HITLWrapper_RecorderFailureFallsBackToBlocking pins that a broken durable store degrades to the blocking ask, never drops the gate.
func TestUnit_HITLWrapper_RecorderFailureFallsBackToBlocking(t *testing.T) {
	inner := &mockInnerTools{}
	policy := newRecordingApprovePolicy()
	policy.recordErr = errors.New("db is gone")
	w := localtools.NewHITLWrapper(inner, alwaysApprove, policy, nil)
	w.SetParkWindow(10 * time.Millisecond)

	res, _, err := execGatedCall(t, w, "call-f")
	require.NoError(t, err)
	require.Equal(t, "ok", res)
	require.Equal(t, []string{"run_command"}, inner.calls, "the human still gated the call, just not durably")
}

// TestUnit_HITLWrapper_NoRecorderKeepsBlockingBehavior pins that an evaluator-only policy blocks past the park window: no row, no suspension.
func TestUnit_HITLWrapper_NoRecorderKeepsBlockingBehavior(t *testing.T) {
	inner := &mockInnerTools{}
	slowApprove := func(ctx context.Context, _ hitlservice.ApprovalRequest) (bool, error) {
		time.Sleep(50 * time.Millisecond)
		return true, nil
	}
	w := localtools.NewHITLWrapper(inner, slowApprove, approvePolicy(), nil)
	w.SetParkWindow(10 * time.Millisecond)

	res, _, err := execGatedCall(t, w, "call-legacy")
	require.NoError(t, err)
	require.Equal(t, "ok", res, "legacy path blocks until the human answers, park window notwithstanding")
	require.Equal(t, []string{"run_command"}, inner.calls)
}

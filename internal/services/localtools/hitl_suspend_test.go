package localtools_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/contenox/contenox/internal/kernel/taskengine"
	"github.com/contenox/contenox/internal/services/hitlservice"
	"github.com/contenox/contenox/internal/services/localtools"
	"github.com/contenox/contenox/internal/store/runtimetypes"
	"github.com/stretchr/testify/require"
)

type recordingApprovePolicy struct {
	result hitlservice.EvaluationResult

	mu        sync.Mutex
	recorded  []string
	requests  []hitlservice.ApprovalRequest
	resolved  map[string]bool
	responded map[string]bool
	recordErr error
}

func (p *recordingApprovePolicy) Evaluate(context.Context, string, string, map[string]any) (hitlservice.EvaluationResult, error) {
	return p.result, nil
}

func (p *recordingApprovePolicy) RecordPendingApproval(_ context.Context, approvalID string, req hitlservice.ApprovalRequest) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.recordErr != nil {
		return p.recordErr
	}
	p.recorded = append(p.recorded, approvalID)
	p.requests = append(p.requests, req)
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

func (p *recordingApprovePolicy) Respond(_ context.Context, approvalID string, approved bool) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.responded == nil {
		p.responded = map[string]bool{}
	}
	p.responded[approvalID] = approved
	return nil
}

func (p *recordingApprovePolicy) verdicts() map[string]bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := map[string]bool{}
	for k, v := range p.responded {
		out[k] = v
	}
	return out
}

func (p *recordingApprovePolicy) inlineVerdicts() map[string]bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := map[string]bool{}
	for k, v := range p.resolved {
		out[k] = v
	}
	return out
}

func newRecordingApprovePolicy() *recordingApprovePolicy {
	return &recordingApprovePolicy{result: hitlservice.EvaluationResult{Action: hitlservice.ActionApprove}}
}

var _ hitlservice.ApprovalRecorder = (*recordingApprovePolicy)(nil)

type noopCheckpointSaver struct{}

func (noopCheckpointSaver) SaveCheckpoint(context.Context, *taskengine.Checkpoint) error {
	return nil
}

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

func suspendableCtx(callID string) context.Context {
	ctx := taskengine.WithSuspendableToolCall(context.Background())
	ctx = taskengine.WithCheckpointSaver(ctx, noopCheckpointSaver{})
	if callID != "" {
		ctx = context.WithValue(ctx, taskengine.ContextKeyToolCallID, callID)
	}
	return ctx
}

func execSuspendableCall(t *testing.T, w *localtools.HITLWrapper, callID string) (any, taskengine.DataType, error) {
	t.Helper()
	return w.Exec(suspendableCtx(callID), time.Now(), map[string]any{"path": "a.txt"}, false,
		&taskengine.ToolsCall{Name: "local_shell", ToolName: "run_command"})
}

func TestUnit_HITLWrapper_ReleasesAtCreation(t *testing.T) {
	inner := &mockInnerTools{}
	policy := newRecordingApprovePolicy()
	asked := make(chan struct{}, 1)
	ask := func(ctx context.Context, req hitlservice.ApprovalRequest) (bool, error) {
		asked <- struct{}{}
		<-ctx.Done()
		return false, ctx.Err()
	}
	w := localtools.NewHITLWrapper(inner, ask, policy, nil)

	_, _, err := execSuspendableCall(t, w, "call-9")

	var pend *taskengine.ApprovalPendingError
	require.ErrorAs(t, err, &pend)
	require.Equal(t, "call-9", pend.ApprovalID, "approval ID == the engine-minted call ID")
	require.Equal(t, "run_command", pend.ToolName)
	require.Equal(t, []string{"call-9"}, policy.recorded, "the durable row precedes the release")
	require.Empty(t, policy.inlineVerdicts(), "the row stays pending — nobody has answered")
	require.Empty(t, inner.calls, "the gated tool must not run")

	select {
	case <-asked:
	case <-time.After(5 * time.Second):
		t.Fatal("the card must still be raised, detached from the released call")
	}
}

func TestUnit_HITLWrapper_VerdictLandsThroughTheResponder(t *testing.T) {
	inner := &mockInnerTools{}
	policy := newRecordingApprovePolicy()
	w := localtools.NewHITLWrapper(inner, alwaysApprove, policy, nil)

	_, _, err := execSuspendableCall(t, w, "call-fast")

	var pend *taskengine.ApprovalPendingError
	require.ErrorAs(t, err, &pend, "an instant answer must not decide whether the ask was durable")
	require.Equal(t, []string{"call-fast"}, policy.recorded)
	require.Empty(t, inner.calls, "the tool runs on the resume, not here")

	require.Eventually(t, func() bool {
		return len(policy.verdicts()) == 1
	}, 5*time.Second, 5*time.Millisecond, "the verdict must reach the durable row through Respond")
	require.Equal(t, map[string]bool{"call-fast": true}, policy.verdicts())
	require.Empty(t, policy.inlineVerdicts(), "a released call never closes its row inline")
}

func TestUnit_HITLWrapper_NoAttachedClientStillCheckpoints(t *testing.T) {
	inner := &mockInnerTools{}
	policy := newRecordingApprovePolicy()
	noClient := func(context.Context, hitlservice.ApprovalRequest) (bool, error) {
		return false, errors.New("no editor session is attached to answer it")
	}
	w := localtools.NewHITLWrapper(inner, noClient, policy, nil)

	_, _, err := execSuspendableCall(t, w, "call-unattended")

	var pend *taskengine.ApprovalPendingError
	require.ErrorAs(t, err, &pend, "nobody to ask is a suspension, not a failed run")
	require.Equal(t, "call-unattended", pend.ApprovalID)
	require.Equal(t, []string{"call-unattended"}, policy.recorded, "the ask is durable even though no client saw it")
	require.Empty(t, inner.calls)

	require.Never(t, func() bool {
		return len(policy.verdicts())+len(policy.inlineVerdicts()) > 0
	}, 200*time.Millisecond, 10*time.Millisecond, "a transport failure is not a verdict")
}

func TestUnit_HITLWrapper_RecordedAskCarriesItsSession(t *testing.T) {
	inner := &mockInnerTools{}
	policy := newRecordingApprovePolicy()
	w := localtools.NewHITLWrapper(inner, blockUntilCtxDone, policy, nil)

	ctx := context.WithValue(suspendableCtx("call-s"), runtimetypes.SessionIDContextKey, "f1084c50-session")
	_, _, err := w.Exec(ctx, time.Now(), map[string]any{"path": "a.txt"}, false,
		&taskengine.ToolsCall{Name: "local_shell", ToolName: "run_command"})

	var pend *taskengine.ApprovalPendingError
	require.ErrorAs(t, err, &pend)
	require.Len(t, policy.requests, 1)
	require.Equal(t, "f1084c50-session", policy.requests[0].SessionID,
		"the released ask must know the session it belongs to")

	bare := newRecordingApprovePolicy()
	wBare := localtools.NewHITLWrapper(&mockInnerTools{}, blockUntilCtxDone, bare, nil)
	_, _, err = execSuspendableCall(t, wBare, "call-bare")
	require.ErrorAs(t, err, &pend)
	require.Len(t, bare.requests, 1)
	require.Empty(t, bare.requests[0].SessionID)
}

func TestUnit_HITLWrapper_InlineVerdictClosesTheRow(t *testing.T) {
	inner := &mockInnerTools{}
	policy := newRecordingApprovePolicy()
	w := localtools.NewHITLWrapper(inner, alwaysApprove, policy, nil)

	res, dt, err := execGatedCall(t, w, "call-1")
	require.NoError(t, err)
	require.Equal(t, "ok", res)
	require.Equal(t, taskengine.DataTypeString, dt)
	require.Equal(t, []string{"run_command"}, inner.calls)
	require.Equal(t, map[string]bool{"call-1": true}, policy.resolved)

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

	ctx2 := context.WithValue(context.Background(), taskengine.ContextKeyToolCallID, "call-8")
	ctx2 = taskengine.WithApprovalVerdicts(ctx2, map[string]bool{"call-8": false})
	res2, _, err := w.Exec(ctx2, time.Now(), map[string]any{}, false,
		&taskengine.ToolsCall{Name: "local_shell", ToolName: "run_command"})
	require.NoError(t, err)
	require.Equal(t, localtools.DenyMessage, res2)
	require.False(t, askCalled)
	require.Len(t, inner.calls, 1, "only the approved call reached the inner repo")

	// A verdict for a DIFFERENT call must not leak onto this one.
	ctx3 := context.WithValue(context.Background(), taskengine.ContextKeyToolCallID, "call-other")
	ctx3 = taskengine.WithApprovalVerdicts(ctx3, map[string]bool{"call-7": true})
	res3, _, err := w.Exec(ctx3, time.Now(), map[string]any{}, false,
		&taskengine.ToolsCall{Name: "local_shell", ToolName: "run_command"})
	require.NoError(t, err)
	require.Equal(t, localtools.DenyMessage, res3)
	require.True(t, askCalled)
}

func TestUnit_HITLWrapper_ShortRuleTimeoutStillSuspends(t *testing.T) {
	inner := &mockInnerTools{}
	policy := newRecordingApprovePolicy()
	policy.result.TimeoutS = 1
	policy.result.OnTimeout = hitlservice.ActionDeny
	w := localtools.NewHITLWrapper(inner, blockUntilCtxDone, policy, nil)

	res, _, err := execSuspendableCall(t, w, "call-t")

	var pend *taskengine.ApprovalPendingError
	require.ErrorAs(t, err, &pend, "a short rule deadline must not collapse the ask into an auto-deny")
	require.Equal(t, "call-t", pend.ApprovalID)
	require.Nil(t, res)
	require.Empty(t, inner.calls)
	require.Equal(t, []string{"call-t"}, policy.recorded)
	require.Empty(t, policy.inlineVerdicts(), "the row stays answerable until its own deadline")
}

// TestUnit_HITLWrapper_RecorderFailureFallsBackToBlocking pins that a broken durable store degrades to the blocking ask, never drops the gate.
func TestUnit_HITLWrapper_RecorderFailureFallsBackToBlocking(t *testing.T) {
	inner := &mockInnerTools{}
	policy := newRecordingApprovePolicy()
	policy.recordErr = errors.New("db is gone")
	w := localtools.NewHITLWrapper(inner, alwaysApprove, policy, nil)

	res, _, err := execSuspendableCall(t, w, "call-f")
	require.NoError(t, err)
	require.Equal(t, "ok", res)
	require.Equal(t, []string{"run_command"}, inner.calls, "the human still gated the call, just not durably")
	require.Empty(t, policy.inlineVerdicts(), "there is no row to close")
}

func TestUnit_HITLWrapper_NoRecorderKeepsBlockingBehavior(t *testing.T) {
	inner := &mockInnerTools{}
	slowApprove := func(ctx context.Context, _ hitlservice.ApprovalRequest) (bool, error) {
		time.Sleep(50 * time.Millisecond)
		return true, nil
	}
	w := localtools.NewHITLWrapper(inner, slowApprove, approvePolicy(), nil)

	res, _, err := execSuspendableCall(t, w, "call-legacy")
	require.NoError(t, err)
	require.Equal(t, "ok", res, "the legacy path blocks until the human answers")
	require.Equal(t, []string{"run_command"}, inner.calls)
}

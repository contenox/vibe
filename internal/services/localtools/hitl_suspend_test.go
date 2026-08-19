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
	rows      map[string]bool // terminal verdicts, as the durable row holds them
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

// ApprovalVerdict reads the durable row, the way hitlservice does.
func (p *recordingApprovePolicy) ApprovalVerdict(_ context.Context, approvalID string) (bool, bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	approved, ok := p.rows[approvalID]
	return approved, ok, nil
}

// resolveRowElsewhere is a verdict written straight to the row by somebody who
// is not this process: a phone over the relay, a second terminal, a quorum flow.
func (p *recordingApprovePolicy) resolveRowElsewhere(approvalID string, approved bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.rows == nil {
		p.rows = map[string]bool{}
	}
	p.rows[approvalID] = approved
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

var (
	_ hitlservice.ApprovalRecorder = (*recordingApprovePolicy)(nil)
	_ hitlservice.ApprovalWatcher  = (*recordingApprovePolicy)(nil)
)

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

// TestUnit_HITLWrapper_AnsweringTheCardContinuesTheTurn is the regression test
// for the reported beam bug: a gated call on an attended surface used to record
// its row, raise a card and suspend in the same instant, so answering the card
// resolved a row whose turn was already gone and the agent stalled forever. The
// row is still written first, but the call now blocks on it — and the answer
// runs the tool and carries the turn on in place.
func TestUnit_HITLWrapper_AnsweringTheCardContinuesTheTurn(t *testing.T) {
	inner := &mockInnerTools{}
	policy := newRecordingApprovePolicy()
	answered := make(chan struct{})
	ask := func(ctx context.Context, req hitlservice.ApprovalRequest) (bool, error) {
		close(answered)
		return true, nil
	}
	w := localtools.NewHITLWrapper(inner, ask, policy, nil)

	res, dt, err := execSuspendableCall(t, w, "call-9")

	var pend *taskengine.ApprovalPendingError
	require.NotErrorAs(t, err, &pend,
		"the reported bug: an attended, checkpointable surface released the turn up front instead of waiting")
	require.NoError(t, err, "an answered ask is not a suspension")
	require.Equal(t, "ok", res)
	require.Equal(t, taskengine.DataTypeString, dt)
	require.Equal(t, []string{"call-9"}, policy.recorded, "the durable row is still written first")
	require.Equal(t, []string{"run_command"}, inner.calls, "the gated tool runs, in this turn")
	require.Equal(t, map[string]bool{"call-9": true}, policy.inlineVerdicts(),
		"the row closes inline — no resume hook may fire for a run that is still alive")
	require.Empty(t, policy.verdicts(), "the card is not a privileged writer: it never calls Respond")
	<-answered
}

// TestUnit_HITLWrapper_VerdictFromAnotherProcessContinuesTheTurn pins the half a
// local card can never cover: the phone over the relay, `contenox approvals
// respond` in a second terminal, or an EE quorum flow writes the verdict onto
// the ROW, and the blocked call must see it there.
func TestUnit_HITLWrapper_VerdictFromAnotherProcessContinuesTheTurn(t *testing.T) {
	inner := &mockInnerTools{}
	policy := newRecordingApprovePolicy()
	// No local card can answer this one at all.
	noClient := func(context.Context, hitlservice.ApprovalRequest) (bool, error) {
		return false, errors.New("no editor session is attached to answer it")
	}
	w := localtools.NewHITLWrapper(inner, noClient, policy, nil)

	go func() {
		require.Eventually(t, func() bool {
			policy.mu.Lock()
			defer policy.mu.Unlock()
			return len(policy.recorded) == 1
		}, 5*time.Second, 5*time.Millisecond)
		policy.resolveRowElsewhere("call-elsewhere", true)
	}()

	res, _, err := execSuspendableCall(t, w, "call-elsewhere")

	require.NoError(t, err)
	require.Equal(t, "ok", res, "a verdict nobody local wrote still releases the call")
	require.Equal(t, []string{"run_command"}, inner.calls, "and the gated tool runs")
}

// TestUnit_HITLWrapper_RowStaysPendingUntilTerminal pins that the wait resolves
// on a terminal verdict only. A row rewritten pending-to-pending — partial
// quorum, reassignment — must not be read as an answer.
func TestUnit_HITLWrapper_RowStaysPendingUntilTerminal(t *testing.T) {
	inner := &mockInnerTools{}
	policy := newRecordingApprovePolicy()
	policy.result.TimeoutS = 2
	policy.result.OnTimeout = hitlservice.ActionDeny
	noClient := func(context.Context, hitlservice.ApprovalRequest) (bool, error) {
		return false, errors.New("no editor session is attached to answer it")
	}
	w := localtools.NewHITLWrapper(inner, noClient, policy, nil)

	res, _, err := execSuspendableCall(t, w, "call-quorum")

	require.NoError(t, err)
	require.Equal(t, localtools.DenyTimeoutMessage, res, "nothing terminal was ever written")
	require.Empty(t, inner.calls)
}

// TestUnit_HITLWrapper_NoAttachedClientWaitsOutTheRow pins that a card nobody
// can present is not a verdict. The row stays open to an answer from anywhere
// else until the operator's wait runs out, and only then does on_timeout stand in.
func TestUnit_HITLWrapper_NoAttachedClientWaitsOutTheRow(t *testing.T) {
	inner := &mockInnerTools{}
	policy := newRecordingApprovePolicy()
	policy.result.TimeoutS = 1
	policy.result.OnTimeout = hitlservice.ActionDeny
	noClient := func(context.Context, hitlservice.ApprovalRequest) (bool, error) {
		return false, errors.New("no editor session is attached to answer it")
	}
	w := localtools.NewHITLWrapper(inner, noClient, policy, nil)

	started := time.Now()
	res, _, err := execSuspendableCall(t, w, "call-unattended")

	require.NoError(t, err, "a transport failure is not a failed run")
	require.Equal(t, localtools.DenyTimeoutMessage, res)
	require.GreaterOrEqual(t, time.Since(started), 900*time.Millisecond,
		"the ask stayed answerable for its whole wait, not just for as long as the card")
	require.Equal(t, []string{"call-unattended"}, policy.recorded, "the ask is durable even though no client saw it")
	require.Equal(t, map[string]bool{"call-unattended": false}, policy.inlineVerdicts(), "on_timeout closed the row")
	require.Empty(t, inner.calls)
}

// TestUnit_HITLWrapper_OnTimeoutAllowRunsTheTool pins the other half of the
// timeout verdict: on_timeout = allow lets the run carry on with the tool run.
func TestUnit_HITLWrapper_OnTimeoutAllowRunsTheTool(t *testing.T) {
	inner := &mockInnerTools{}
	policy := newRecordingApprovePolicy()
	policy.result.TimeoutS = 1
	policy.result.OnTimeout = hitlservice.ActionAllow
	w := localtools.NewHITLWrapper(inner, blockUntilCtxDone, policy, nil)

	res, _, err := execSuspendableCall(t, w, "call-allow")

	require.NoError(t, err)
	require.Equal(t, "ok", res)
	require.Equal(t, []string{"run_command"}, inner.calls)
	require.Equal(t, map[string]bool{"call-allow": true}, policy.inlineVerdicts())
}

// TestUnit_HITLWrapper_ProcessLeavingSuspends pins the one non-detached
// suspension left: the process is actually going away, so the row stays pending
// and the engine checkpoints beside it for a resume elsewhere.
func TestUnit_HITLWrapper_ProcessLeavingSuspends(t *testing.T) {
	inner := &mockInnerTools{}
	policy := newRecordingApprovePolicy()
	w := localtools.NewHITLWrapper(inner, blockUntilCtxDone, policy, nil)

	ctx, cancel := context.WithCancel(suspendableCtx("call-shutdown"))
	go func() {
		require.Eventually(t, func() bool {
			policy.mu.Lock()
			defer policy.mu.Unlock()
			return len(policy.recorded) == 1
		}, 5*time.Second, 5*time.Millisecond)
		cancel()
	}()
	defer cancel()

	_, _, err := w.Exec(ctx, time.Now(), map[string]any{"path": "a.txt"}, false,
		&taskengine.ToolsCall{Name: "local_shell", ToolName: "run_command"})

	var pend *taskengine.ApprovalPendingError
	require.ErrorAs(t, err, &pend, "a dying process suspends rather than inventing a verdict")
	require.Equal(t, "call-shutdown", pend.ApprovalID)
	require.Empty(t, policy.inlineVerdicts(), "the row stays pending — it is what makes the resume possible")
	require.Empty(t, inner.calls)
}

// TestUnit_HITLWrapper_DetachedAsksRelease pins the only up-front release left:
// a caller that explicitly declared nobody is attached to answer this run.
func TestUnit_HITLWrapper_DetachedAsksRelease(t *testing.T) {
	inner := &mockInnerTools{}
	policy := newRecordingApprovePolicy()
	w := localtools.NewHITLWrapper(inner, alwaysApprove, policy, nil)

	ctx := taskengine.WithDetachedAsks(suspendableCtx("call-detached"))
	_, _, err := w.Exec(ctx, time.Now(), map[string]any{"path": "a.txt"}, false,
		&taskengine.ToolsCall{Name: "local_shell", ToolName: "run_command"})

	var pend *taskengine.ApprovalPendingError
	require.ErrorAs(t, err, &pend)
	require.Equal(t, "call-detached", pend.ApprovalID)
	require.Equal(t, []string{"call-detached"}, policy.recorded)
	require.Empty(t, inner.calls, "the tool runs on the resume, not here")

	require.Eventually(t, func() bool {
		return len(policy.verdicts()) == 1
	}, 5*time.Second, 5*time.Millisecond, "a detached card answers through Respond, which is what fires the resume hook")
	require.Equal(t, map[string]bool{"call-detached": true}, policy.verdicts())
	require.Empty(t, policy.inlineVerdicts(), "a released call never closes its row inline")
}

func TestUnit_HITLWrapper_RecordedAskCarriesItsSession(t *testing.T) {
	inner := &mockInnerTools{}
	policy := newRecordingApprovePolicy()
	w := localtools.NewHITLWrapper(inner, alwaysDeny, policy, nil)

	ctx := context.WithValue(suspendableCtx("call-s"), runtimetypes.SessionIDContextKey, "f1084c50-session")
	_, _, err := w.Exec(ctx, time.Now(), map[string]any{"path": "a.txt"}, false,
		&taskengine.ToolsCall{Name: "local_shell", ToolName: "run_command"})

	require.NoError(t, err)
	require.Len(t, policy.requests, 1)
	require.Equal(t, "f1084c50-session", policy.requests[0].SessionID,
		"the recorded ask must know the session it belongs to")

	bare := newRecordingApprovePolicy()
	wBare := localtools.NewHITLWrapper(&mockInnerTools{}, alwaysDeny, bare, nil)
	_, _, err = execSuspendableCall(t, wBare, "call-bare")
	require.NoError(t, err)
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

func TestUnit_HITLWrapper_ShortRuleWaitAppliesOnTimeout(t *testing.T) {
	inner := &mockInnerTools{}
	policy := newRecordingApprovePolicy()
	policy.result.TimeoutS = 1
	policy.result.OnTimeout = hitlservice.ActionDeny
	w := localtools.NewHITLWrapper(inner, blockUntilCtxDone, policy, nil)

	res, _, err := execSuspendableCall(t, w, "call-t")

	require.NoError(t, err, "a wait that runs out is a verdict, not a failed run")
	require.Equal(t, localtools.DenyTimeoutMessage, res)
	require.Empty(t, inner.calls)
	require.Equal(t, []string{"call-t"}, policy.recorded)
	require.Equal(t, map[string]bool{"call-t": false}, policy.inlineVerdicts(),
		"the row closes on the same verdict the run carried on with")
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

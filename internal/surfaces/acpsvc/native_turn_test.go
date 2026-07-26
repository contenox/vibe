package acpsvc

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/contenox/beam/internal/kernel/nativeturn"
	"github.com/contenox/beam/internal/kernel/taskengine"
	"github.com/contenox/beam/internal/services/agentservice"
	"github.com/contenox/beam/libacp"
	"github.com/stretchr/testify/require"
)

// TestUnit_NativeEventTranslator_EmitsExpected verifies the survival-path
// translator produces the same session/update vocabulary Transport.publishEvent
// does, with turn-local tool-call invocation sequencing and the token-size
// fallback.
func TestUnit_NativeEventTranslator_EmitsExpected(t *testing.T) {
	var got []libacp.SessionNotification
	tr := newNativeEventTranslator(
		func(_ context.Context, n libacp.SessionNotification) { got = append(got, n) },
		func() int { return 200 }, // token-size fallback
	)
	const sid = libacp.SessionID("s")
	emit := func(ev taskengine.TaskEvent) {
		raw, err := json.Marshal(ev)
		require.NoError(t, err)
		tr.publish(context.Background(), sid, raw)
	}

	emit(taskengine.TaskEvent{Kind: taskengine.TaskEventPrint, Content: "hi"})
	// TokenSize 0 falls back to the session budget (200).
	emit(taskengine.TaskEvent{Kind: taskengine.TaskEventTokenUsage, TokenUsed: 5})
	// A tool call with no ApprovalID: first invocation keeps the bare name.
	emit(taskengine.TaskEvent{Kind: taskengine.TaskEventToolCallPending, ToolName: "grep", TaskID: "t1"})
	emit(taskengine.TaskEvent{Kind: taskengine.TaskEventToolCall, ToolName: "grep", TaskID: "t1", Content: "\"done\""})
	// Second invocation of the same tool gets a distinct invocation id (#2).
	emit(taskengine.TaskEvent{Kind: taskengine.TaskEventToolCallPending, ToolName: "grep", TaskID: "t1"})

	require.Len(t, got, 5)

	require.Equal(t, libacp.SessionUpdateAgentMessageChunk, got[0].Update.SessionUpdate)
	require.Equal(t, "hi", got[0].Update.Content.Text)

	require.Equal(t, libacp.SessionUpdateUsageUpdate, got[1].Update.SessionUpdate)
	require.Equal(t, 5, got[1].Update.Used)
	require.Equal(t, 200, got[1].Update.Size, "token size must fall back to the session budget")

	require.Equal(t, libacp.SessionUpdateToolCall, got[2].Update.SessionUpdate)
	require.Equal(t, "grep", got[2].Update.ToolCallID)

	require.Equal(t, libacp.SessionUpdateToolCallUpdate, got[3].Update.SessionUpdate)
	require.Equal(t, "grep", got[3].Update.ToolCallID, "the closing update reuses the open invocation id")

	require.Equal(t, libacp.SessionUpdateToolCall, got[4].Update.SessionUpdate)
	require.Equal(t, "grep#2", got[4].Update.ToolCallID, "a second invocation is a distinct card")
}

// TestUnit_PermissionPendingGuard verifies the per-connection suppression flag the
// native-turn viewer consults on delivery.
func TestUnit_PermissionPendingGuard(t *testing.T) {
	tr := &Transport{}
	const sid = libacp.SessionID("s")
	require.False(t, tr.isPermissionPending(sid, "call-1"))
	tr.markPermissionPending(sid, "call-1")
	require.True(t, tr.isPermissionPending(sid, "call-1"))
	require.False(t, tr.isPermissionPending(sid, "call-2"))
	tr.clearPermissionPending(sid, "call-1")
	require.False(t, tr.isPermissionPending(sid, "call-1"))
}

// blockingAgent is a fakeAgent whose Prompt blocks until released or its context is
// cancelled — enough to drive the survival lifecycle deterministically.
type blockingAgent struct {
	*fakeAgent
	startOnce sync.Once
	started   chan struct{}
	release   chan struct{}
}

func newBlockingAgent() *blockingAgent {
	return &blockingAgent{fakeAgent: &fakeAgent{}, started: make(chan struct{}), release: make(chan struct{})}
}

func (b *blockingAgent) Prompt(ctx context.Context, req agentservice.PromptRequest) (*agentservice.PromptResponse, error) {
	b.startOnce.Do(func() { close(b.started) })
	select {
	case <-b.release:
		return &agentservice.PromptResponse{StopReason: agentservice.StopEndTurn}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// transportWithRegistry wires the fakeAgent harness with a live native-turn
// Registry and a connection context, so the survival path (promptViaRegistry) runs.
func transportWithRegistry(a agentservice.Agent) (*Transport, libacp.SessionID, *nativeturn.Registry) {
	tr, sid, _ := transportWithFakeAgent(a)
	reg := nativeturn.New(nativeturn.Config{TurnDeadline: 5 * time.Second, GraceWindow: 5 * time.Second})
	tr.deps.NativeTurns = reg
	tr.connCtx, tr.connCancel = context.WithCancel(context.Background())
	// Skip the runtime-state token-budget fallback (no State in this harness).
	tr.sessions[sid].EffectiveTokenLimit = 1000
	return tr, sid, reg
}

type promptOutcome struct {
	resp libacp.PromptResponse
	err  error
}

// TestSurvival_PromptSurvivesConnectionDrop is the acpsvc end of the bug fix: a
// connection drop makes the connected Prompt return WITHOUT cancelling the turn,
// which keeps running on the Registry and completes on its own.
func TestSurvival_PromptSurvivesConnectionDrop(t *testing.T) {
	agent := newBlockingAgent()
	tr, sid, reg := transportWithRegistry(agent)
	defer reg.Close()

	sess := tr.sessions[sid]
	d := sess.driver.(*nativeDriver)

	out := make(chan promptOutcome, 1)
	go func() {
		resp, err := d.Prompt(context.Background(), promptReq(sid), sess)
		out <- promptOutcome{resp, err}
	}()

	<-agent.started // the turn is running agent.Prompt on the Registry
	st, ok := reg.Get(sid)
	require.True(t, ok)
	require.Equal(t, nativeturn.StateRunning, st.State)
	require.Equal(t, 1, st.Viewers)

	// Drop the connection. The connected Prompt must return promptly.
	tr.connCancel()
	select {
	case res := <-out:
		require.NoError(t, res.err)
	case <-time.After(2 * time.Second):
		t.Fatal("Prompt did not return after connection drop")
	}

	// The turn was NOT cancelled: it is still present (now unwatched, in grace) and
	// the agent is still blocked, not released or ctx-cancelled.
	st, ok = reg.Get(sid)
	require.True(t, ok, "turn must survive the connection drop")
	require.Equal(t, nativeturn.StateGrace, st.State)
	require.Equal(t, 0, st.Viewers)

	// Let it finish on its own; it then tears down.
	close(agent.release)
	require.Eventually(t, func() bool { _, ok := reg.Get(sid); return !ok }, 2*time.Second, 5*time.Millisecond,
		"the surviving turn should complete and be reclaimed")
}

// TestSurvival_PromptCompletesWhileConnected verifies the happy path: the connected
// Prompt resolves with the turn's stop reason when it finishes.
func TestSurvival_PromptCompletesWhileConnected(t *testing.T) {
	agent := newBlockingAgent()
	tr, sid, reg := transportWithRegistry(agent)
	defer reg.Close()

	sess := tr.sessions[sid]
	d := sess.driver.(*nativeDriver)

	out := make(chan promptOutcome, 1)
	go func() {
		resp, err := d.Prompt(context.Background(), promptReq(sid), sess)
		out <- promptOutcome{resp, err}
	}()

	<-agent.started
	close(agent.release) // let the turn complete

	select {
	case res := <-out:
		require.NoError(t, res.err)
		require.Equal(t, libacp.StopReasonEndTurn, res.resp.StopReason)
	case <-time.After(2 * time.Second):
		t.Fatal("Prompt did not resolve on turn completion")
	}
	require.Eventually(t, func() bool { _, ok := reg.Get(sid); return !ok }, 2*time.Second, 5*time.Millisecond)
}

// TestSurvival_SessionCancelEndsTurn verifies session/cancel (Transport.Cancel)
// reaches the Registry turn — the only real user cancel — and resolves the prompt
// as cancelled.
func TestSurvival_SessionCancelEndsTurn(t *testing.T) {
	agent := newBlockingAgent()
	tr, sid, reg := transportWithRegistry(agent)
	defer reg.Close()

	sess := tr.sessions[sid]
	d := sess.driver.(*nativeDriver)

	out := make(chan promptOutcome, 1)
	go func() {
		resp, err := d.Prompt(context.Background(), promptReq(sid), sess)
		out <- promptOutcome{resp, err}
	}()

	<-agent.started
	require.NoError(t, tr.Cancel(context.Background(), libacp.CancelNotification{SessionID: sid}))

	select {
	case res := <-out:
		require.NoError(t, res.err, "a cancel resolves with no JSON-RPC error, per the ACP contract")
		require.Equal(t, libacp.StopReasonCancelled, res.resp.StopReason)
	case <-time.After(2 * time.Second):
		t.Fatal("Prompt did not resolve after session/cancel")
	}
	_, ok := reg.Get(sid)
	require.False(t, ok, "the cancelled turn must be torn down")
}

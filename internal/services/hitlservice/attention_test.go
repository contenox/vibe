package hitlservice_test

import (
	"testing"
	"time"

	"github.com/contenox/beam/internal/kernel/taskengine"
	"github.com/contenox/beam/internal/services/hitlservice"
	"github.com/contenox/beam/internal/store/runtimetypes"
	"github.com/stretchr/testify/require"
)

// TestUnit_RequestAttention_ReturnsTheOperatorsWords is the keystone of the ask
// channel: a unit's question parks until a human answers it, and what comes back
// is the human's TEXT — not a boolean, not "someone was notified". A unit that
// learns only that it was heard still cannot proceed; a unit handed the answer
// finishes on its next turn.
func TestUnit_RequestAttention_ReturnsTheOperatorsWords(t *testing.T) {
	ctx, store, _ := setupHITLDB(t)
	svc := newDurableService(t, store)

	answered := make(chan string, 1)
	go func() {
		text, err := svc.RequestAttention(ctx, hitlservice.AttentionRequest{
			Summary:   "which project did you mean?",
			Detail:    "the intent named none",
			MissionID: "m-1",
			AgentName: "chain-acp",
		}, taskengine.NoopTaskEventSink{})
		require.NoError(t, err)
		answered <- text
	}()

	// The question is durably pending and identifiable as one that needs data.
	var ask *runtimetypes.HITLApproval
	require.Eventually(t, func() bool {
		rows, err := svc.ListPending(ctx, 10)
		require.NoError(t, err)
		if len(rows) != 1 {
			return false
		}
		ask = rows[0]
		return true
	}, 5*time.Second, 10*time.Millisecond, "the unit's question must be listed as a pending ask")

	require.True(t, hitlservice.IsAttentionAsk(ask),
		"the row must be recognisable as a question, not a permission gate — answering it with a bare approve would leave the unit with nothing")
	require.Equal(t, "which project did you mean?", ask.ArgsSummary)
	require.NotNil(t, ask.MissionID)
	require.Equal(t, "m-1", *ask.MissionID)

	require.NoError(t, svc.Answer(ctx, ask.ID, "the contenox runtime repo at /home/x/src/contenox"))

	select {
	case text := <-answered:
		require.Equal(t, "the contenox runtime repo at /home/x/src/contenox", text)
	case <-time.After(5 * time.Second):
		t.Fatal("the asking unit was never woken with the answer")
	}

	// And the answer is durable: a restart still shows what was told to the unit.
	resolved, err := store.GetHITLApproval(ctx, ask.ID)
	require.NoError(t, err)
	require.Equal(t, runtimetypes.HITLApprovalApproved, resolved.State)
	require.Equal(t, "the contenox runtime repo at /home/x/src/contenox", hitlservice.AnswerOf(resolved))
}

// TestUnit_Answer_RefusesAPermissionAsk guards the one confusion the two ask
// kinds could cause: a permission ask is a yes/no gate, so resolving it with
// prose would close it with no verdict at all.
func TestUnit_Answer_RefusesAPermissionAsk(t *testing.T) {
	ctx, store, _ := setupHITLDB(t)
	svc := newDurableService(t, store)

	go func() {
		_, _ = svc.RequestApproval(ctx, hitlservice.ApprovalRequest{
			ToolsName: "local_fs",
			ToolName:  "write_file",
			Args:      map[string]any{"path": "/tmp/x"},
		}, taskengine.NoopTaskEventSink{})
	}()

	var ask *runtimetypes.HITLApproval
	require.Eventually(t, func() bool {
		rows, err := svc.ListPending(ctx, 10)
		require.NoError(t, err)
		if len(rows) != 1 {
			return false
		}
		ask = rows[0]
		return true
	}, 5*time.Second, 10*time.Millisecond)

	require.False(t, hitlservice.IsAttentionAsk(ask))
	err := svc.Answer(ctx, ask.ID, "go ahead")
	require.Error(t, err)
	require.Contains(t, err.Error(), "approve/deny")
}

// TestUnit_Answer_RejectsEmptyText keeps the channel honest in the other
// direction: an empty answer is not an answer, and would wake the unit with
// nothing to act on.
func TestUnit_Answer_RejectsEmptyText(t *testing.T) {
	ctx, store, _ := setupHITLDB(t)
	svc := newDurableService(t, store)
	require.Error(t, svc.Answer(ctx, "any-id", "   "))
}

// TestUnit_Answer_UnknownIDReturnsNotFound mirrors Respond's contract: an ask
// that cannot be answered says so rather than silently doing nothing.
func TestUnit_Answer_UnknownIDReturnsNotFound(t *testing.T) {
	ctx, store, _ := setupHITLDB(t)
	svc := newDurableService(t, store)
	require.ErrorIs(t, svc.Answer(ctx, "no-such-ask", "text"), hitlservice.ErrApprovalNotFound)
}

// TestUnit_RequestAttention_CeilingEndsTheWait pins the bound: an operator who
// never answers must not park a unit forever. The caller gets
// ErrAttentionUnanswered, which is its cue to fall back (missiontools files the
// question as a durable blocker) rather than hang.
func TestUnit_RequestAttention_CeilingEndsTheWait(t *testing.T) {
	ctx, store, _ := setupHITLDB(t)
	svc := newDurableService(t, store)
	hitlservice.SetApprovalCeiling(svc, 150*time.Millisecond)

	start := time.Now()
	text, err := svc.RequestAttention(ctx, hitlservice.AttentionRequest{
		Summary:   "anyone there?",
		MissionID: "m-2",
	}, taskengine.NoopTaskEventSink{})
	require.ErrorIs(t, err, hitlservice.ErrAttentionUnanswered)
	require.Empty(t, text)
	require.Less(t, time.Since(start), 5*time.Second, "the wait must be bounded by the ceiling")
}

// TestUnit_RequestAttention_RequiresASummary refuses a question with nothing in
// it — the row's summary is the only thing an operator sees in a list.
func TestUnit_RequestAttention_RequiresASummary(t *testing.T) {
	ctx, store, _ := setupHITLDB(t)
	svc := newDurableService(t, store)
	_, err := svc.RequestAttention(ctx, hitlservice.AttentionRequest{Summary: "  "}, taskengine.NoopTaskEventSink{})
	require.Error(t, err)
}

// TestUnit_RequestAttention_AnswerFromAnotherProcessWakesTheUnit is the case the
// whole polling half exists for, and the one a channel alone cannot serve: the
// dispatched UNIT raises its question in its own process, while the operator
// answers it in the process that owns the API (`contenox serve`). Two processes,
// one shared SQLite file, no shared memory.
//
// It is simulated exactly as the durable-restart tests do it — a second service
// over the SAME on-disk database, whose `pending` map knows nothing about the
// waiter parked in the first.
func TestUnit_RequestAttention_AnswerFromAnotherProcessWakesTheUnit(t *testing.T) {
	ctx, store, dbPath := setupHITLDB(t)
	unitSvc := newDurableService(t, store)

	answered := make(chan string, 1)
	go func() {
		text, err := unitSvc.RequestAttention(ctx, hitlservice.AttentionRequest{
			Summary:   "which project did you mean?",
			MissionID: "m-cross",
		}, taskengine.NoopTaskEventSink{})
		require.NoError(t, err)
		answered <- text
	}()

	// The operator's process: a different service instance over the same file.
	operatorCtx, operatorStore := reopenHITLDB(t, dbPath)
	operatorSvc := newDurableService(t, operatorStore)

	var askID string
	require.Eventually(t, func() bool {
		rows, err := operatorSvc.ListPending(operatorCtx, 10)
		require.NoError(t, err)
		if len(rows) != 1 {
			return false
		}
		askID = rows[0].ID
		return true
	}, 5*time.Second, 10*time.Millisecond, "the other process must see the question as pending")

	require.NoError(t, operatorSvc.Answer(operatorCtx, askID, "the runtime repo"))

	select {
	case text := <-answered:
		require.Equal(t, "the runtime repo", text)
	case <-time.After(10 * time.Second):
		t.Fatal("a unit waiting in another process was never woken by the answer")
	}
}

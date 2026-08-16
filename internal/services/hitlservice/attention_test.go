package hitlservice_test

import (
	"context"
	"testing"
	"time"

	"github.com/contenox/contenox/internal/kernel/taskengine"
	"github.com/contenox/contenox/internal/services/hitlservice"
	"github.com/contenox/contenox/internal/store/runtimetypes"
	"github.com/stretchr/testify/require"
)

// TestUnit_RequestAttention_ReturnsTheOperatorsWords pins that RequestAttention
// returns the operator's own text, not a boolean.
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

	resolved, err := store.GetHITLApproval(ctx, ask.ID)
	require.NoError(t, err)
	require.Equal(t, runtimetypes.HITLApprovalApproved, resolved.State)
	require.Equal(t, "the contenox runtime repo at /home/x/src/contenox", hitlservice.AnswerOf(resolved))
}

// TestUnit_Answer_RefusesAPermissionAsk pins that Answer rejects a permission ask.
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

// TestUnit_Answer_RejectsEmptyText pins that an empty answer is rejected, not a no-op success.
func TestUnit_Answer_RejectsEmptyText(t *testing.T) {
	ctx, store, _ := setupHITLDB(t)
	svc := newDurableService(t, store)
	require.Error(t, svc.Answer(ctx, "any-id", "   "))
}

// TestUnit_Answer_UnknownIDReturnsNotFound mirrors Respond's contract for an unanswerable ask.
func TestUnit_Answer_UnknownIDReturnsNotFound(t *testing.T) {
	ctx, store, _ := setupHITLDB(t)
	svc := newDurableService(t, store)
	require.ErrorIs(t, svc.Answer(ctx, "no-such-ask", "text"), hitlservice.ErrApprovalNotFound)
}

// TestUnit_RequestAttention_CeilingEndsTheWait pins that an unanswered ask is bounded by the ceiling, not left to hang.
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

// TestUnit_RequestAttention_RequiresASummary pins that a blank summary is rejected.
func TestUnit_RequestAttention_RequiresASummary(t *testing.T) {
	ctx, store, _ := setupHITLDB(t)
	svc := newDurableService(t, store)
	_, err := svc.RequestAttention(ctx, hitlservice.AttentionRequest{Summary: "  "}, taskengine.NoopTaskEventSink{})
	require.Error(t, err)
}

// TestUnit_RequestAttention_AnswerFromAnotherProcessWakesTheUnit pins that an
// answer from a second service instance over the same store wakes the waiter.
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

func TestUnit_RequestAttention_SuspendableCallerReleasesAtCreation(t *testing.T) {
	ctx, store, _ := setupHITLDB(t)
	svc := newDurableService(t, store)

	_, err := svc.RequestAttention(ctx, hitlservice.AttentionRequest{
		Summary: "anyone there?",
		AskID:   "call-ask1",
	}, taskengine.NoopTaskEventSink{})

	var pending *hitlservice.AttentionPendingError
	require.ErrorAs(t, err, &pending)
	require.Equal(t, "call-ask1", pending.AskID, "the pending error names the row to resume on")

	row, err := store.GetHITLApproval(ctx, "call-ask1")
	require.NoError(t, err)
	require.Equal(t, runtimetypes.HITLApprovalPending, row.State, "releasing must leave the question answerable")
}

func raiseAsk(t *testing.T, ctx context.Context, svc hitlservice.Service, missionID, askID, summary string) {
	t.Helper()
	_, err := svc.RequestAttention(ctx, hitlservice.AttentionRequest{
		Summary:   summary,
		MissionID: missionID,
		AskID:     askID,
	}, taskengine.NoopTaskEventSink{})
	var pending *hitlservice.AttentionPendingError
	require.ErrorAs(t, err, &pending, "the released ask must be left pending and answerable")
}

// TestUnit_AnswerAsAgentNamed_RecordsNameAndCountsAgainstBound pins that the
// durable record shows WHO answered, and AgentAnswerCount counts exactly the
// non-human answers.
func TestUnit_AnswerAsAgentNamed_RecordsNameAndCountsAgainstBound(t *testing.T) {
	ctx, store, _ := setupHITLDB(t)
	svc := newDurableService(t, store)
	const missionID = "m-named"

	raiseAsk(t, ctx, svc, missionID, "ask-named", "which repo?")
	raiseAsk(t, ctx, svc, missionID, "ask-generic", "which branch?")
	raiseAsk(t, ctx, svc, missionID, "ask-human", "should I delete it?")

	require.NoError(t, svc.AnswerAsAgentNamed(ctx, "ask-named", "attention-reviewer", "the runtime repo"))
	require.NoError(t, svc.AnswerAsAgent(ctx, "ask-generic", "main"))
	require.NoError(t, svc.Answer(ctx, "ask-human", "no — keep it"))

	named, err := store.GetHITLApproval(ctx, "ask-named")
	require.NoError(t, err)
	require.Equal(t, runtimetypes.HITLApprovalApproved, named.State)
	require.Equal(t, "the runtime repo", hitlservice.AnswerOf(named))
	require.Equal(t, "attention-reviewer", hitlservice.AnsweredByOf(named),
		"the durable record must name the answering agent, not only that an agent answered")

	generic, err := store.GetHITLApproval(ctx, "ask-generic")
	require.NoError(t, err)
	require.Equal(t, "agent", hitlservice.AnsweredByOf(generic))

	human, err := store.GetHITLApproval(ctx, "ask-human")
	require.NoError(t, err)
	require.Empty(t, hitlservice.AnsweredByOf(human), "a human answer records no non-human actor")

	count, err := svc.AgentAnswerCount(ctx, missionID)
	require.NoError(t, err)
	require.Equal(t, 2, count, "named and generic agent answers count against the envelope's bound; the human one does not")
}

// TestUnit_AnswerAsAgentNamed_BlankNameDegradesToGenericMarker pins that a
// blank name never records an empty actor (which would read as human).
func TestUnit_AnswerAsAgentNamed_BlankNameDegradesToGenericMarker(t *testing.T) {
	ctx, store, _ := setupHITLDB(t)
	svc := newDurableService(t, store)
	raiseAsk(t, ctx, svc, "m-blank", "ask-blank", "which port?")

	require.NoError(t, svc.AnswerAsAgentNamed(ctx, "ask-blank", "   ", "8080"))
	row, err := store.GetHITLApproval(ctx, "ask-blank")
	require.NoError(t, err)
	require.Equal(t, "agent", hitlservice.AnsweredByOf(row))

	count, err := svc.AgentAnswerCount(ctx, "m-blank")
	require.NoError(t, err)
	require.Equal(t, 1, count)
}

func TestUnit_RequestAttention_CallerChosenAskIDIsTheRowID(t *testing.T) {
	ctx, store, _ := setupHITLDB(t)
	svc := newDurableService(t, store)

	_, err := svc.RequestAttention(ctx, hitlservice.AttentionRequest{
		Summary: "which branch?",
		AskID:   "call-identity",
	}, taskengine.NoopTaskEventSink{})
	var pending *hitlservice.AttentionPendingError
	require.ErrorAs(t, err, &pending)
	require.Equal(t, "call-identity", pending.AskID)

	row, err := store.GetHITLApproval(ctx, "call-identity")
	require.NoError(t, err)
	require.Equal(t, runtimetypes.HITLApprovalPending, row.State, "the row must exist under the caller's ID")

	require.NoError(t, svc.Answer(ctx, "call-identity", "main"))
	row, err = store.GetHITLApproval(ctx, "call-identity")
	require.NoError(t, err)
	require.Equal(t, "main", hitlservice.AnswerOf(row))
}

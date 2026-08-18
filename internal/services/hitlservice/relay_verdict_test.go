package hitlservice_test

import (
	"context"
	"testing"

	"github.com/contenox/contenox/internal/services/hitlservice"
	"github.com/contenox/contenox/internal/store/runtimetypes"
	"github.com/stretchr/testify/require"
)

func TestUnit_RespondWithGuidance_RecordsWhoDecidedAndWhatTheyAsked(t *testing.T) {
	ctx, store, _ := setupHITLDB(t)
	svc := newDurableService(t, store)
	recorder := svc.(hitlservice.ApprovalRecorder)

	require.NoError(t, recorder.RecordPendingApproval(ctx, "ask-guided", hitlservice.ApprovalRequest{
		ToolsName: "billing",
		ToolName:  "issue_refund",
		Args:      map[string]any{"path": "/tmp/refund.json"},
	}))
	require.NoError(t, svc.RespondWithGuidance(ctx, "ask-guided", false, "u_9", "Refund only up to 40 EUR."))

	row, err := store.GetHITLApproval(ctx, "ask-guided")
	require.NoError(t, err)
	require.Equal(t, runtimetypes.HITLApprovalDenied, row.State)
	require.Equal(t, "u_9", hitlservice.DecidedByOf(row))
	require.Equal(t, "Refund only up to 40 EUR.", hitlservice.GuidanceOf(row))

	reader, ok := svc.(interface {
		AskGuidance(ctx context.Context, approvalID string) (string, string)
	})
	require.True(t, ok, "the gate reads the note back through AskGuidance")
	by, guidance := reader.AskGuidance(ctx, "ask-guided")
	require.Equal(t, "u_9", by)
	require.Equal(t, "Refund only up to 40 EUR.", guidance)
}

func TestUnit_RespondWithGuidance_WithNoActorIsAPlainVerdict(t *testing.T) {
	ctx, store, _ := setupHITLDB(t)
	svc := newDurableService(t, store)
	recorder := svc.(hitlservice.ApprovalRecorder)

	require.NoError(t, recorder.RecordPendingApproval(ctx, "ask-plain", hitlservice.ApprovalRequest{
		ToolsName: "billing", ToolName: "issue_refund",
	}))
	require.NoError(t, svc.RespondWithGuidance(ctx, "ask-plain", true, "  ", ""))

	row, err := store.GetHITLApproval(ctx, "ask-plain")
	require.NoError(t, err)
	require.Equal(t, runtimetypes.HITLApprovalApproved, row.State)
	require.Empty(t, hitlservice.DecidedByOf(row))
	require.True(t, decodeApproved(t, row.Resolution))
}

func TestUnit_AnswerFrom_RecordsTheActorThatAnswered(t *testing.T) {
	ctx, store, _ := setupHITLDB(t)
	svc := newDurableService(t, store)

	answered, err := svc.RequestAttention(ctx, hitlservice.AttentionRequest{
		Summary:   "Which price table applies?",
		MissionID: "m-1",
		AskID:     "ask-attention",
	}, nil)
	require.Empty(t, answered)
	var pending *hitlservice.AttentionPendingError
	require.ErrorAs(t, err, &pending)

	require.NoError(t, svc.AnswerFrom(ctx, "ask-attention", "use the 2019 table", "u_9"))

	row, err := store.GetHITLApproval(ctx, "ask-attention")
	require.NoError(t, err)
	require.Equal(t, "use the 2019 table", hitlservice.AnswerOf(row))
	require.Equal(t, "u_9", hitlservice.AnsweredByOf(row))
}

// TestUnit_RelayedHumanVerdict_SpendsNoAgentBound pins the accounting split: a
// person answering over the relay is attributed like anyone else, but the
// mission's agent quota exists to bound what an agent may decide unattended, and
// a human decision is not that.
func TestUnit_RelayedHumanVerdict_SpendsNoAgentBound(t *testing.T) {
	ctx, store, _ := setupHITLDB(t)
	svc := newDurableService(t, store)
	recorder := svc.(hitlservice.ApprovalRecorder)
	const missionID = "m-relayed"

	require.NoError(t, recorder.RecordPendingApproval(ctx, "ask-relayed", hitlservice.ApprovalRequest{
		ToolsName: "billing",
		ToolName:  "issue_refund",
		MissionID: missionID,
	}))
	require.NoError(t, svc.RespondWithGuidance(ctx, "ask-relayed", false, "u_9", "Refund only up to 40 EUR."))

	_, err := svc.RequestAttention(ctx, hitlservice.AttentionRequest{
		Summary:   "Which price table applies?",
		MissionID: missionID,
		AskID:     "ask-relayed-question",
	}, nil)
	var pending *hitlservice.AttentionPendingError
	require.ErrorAs(t, err, &pending)
	require.NoError(t, svc.AnswerFrom(ctx, "ask-relayed-question", "the 2019 table", "u_9"))

	approvals, err := svc.AgentApprovalCount(ctx, missionID)
	require.NoError(t, err)
	require.Zero(t, approvals, "a relayed human verdict must not spend the mission's agent-approval bound")

	answers, err := svc.AgentAnswerCount(ctx, missionID)
	require.NoError(t, err)
	require.Zero(t, answers, "a relayed human answer must not spend the mission's agent-answer bound")

	// The bound still holds against the actor it was written for.
	require.NoError(t, recorder.RecordPendingApproval(ctx, "ask-adjudicated", hitlservice.ApprovalRequest{
		ToolsName: "billing",
		ToolName:  "issue_refund",
		MissionID: missionID,
	}))
	require.NoError(t, svc.RespondAsAgentBounded(ctx, "ask-adjudicated", "oracle", false, "not in the intent", 1))
	approvals, err = svc.AgentApprovalCount(ctx, missionID)
	require.NoError(t, err)
	require.Equal(t, 1, approvals)

	require.NoError(t, recorder.RecordPendingApproval(ctx, "ask-adjudicated-2", hitlservice.ApprovalRequest{
		ToolsName: "billing",
		ToolName:  "issue_refund",
		MissionID: missionID,
	}))
	require.ErrorIs(t, svc.RespondAsAgentBounded(ctx, "ask-adjudicated-2", "oracle", false, "", 1),
		hitlservice.ErrAgentApprovalBoundSpent)
}

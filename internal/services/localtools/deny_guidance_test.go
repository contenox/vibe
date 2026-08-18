package localtools

import (
	"context"
	"testing"
	"time"

	"github.com/contenox/contenox/internal/kernel/taskengine"
	"github.com/contenox/contenox/internal/services/hitlservice"
	"github.com/contenox/contenox/internal/services/missiontools"
	"github.com/stretchr/testify/require"
)

type stubGuidance struct {
	by, guidance string
}

func (s stubGuidance) RecordPendingApproval(context.Context, string, hitlservice.ApprovalRequest) error {
	return nil
}
func (s stubGuidance) ResolveApprovalInline(context.Context, string, bool) error { return nil }
func (s stubGuidance) AskGuidance(context.Context, string) (string, string) {
	return s.by, s.guidance
}

// TestUnit_DenyMessage_CarriesTheAdjudicatorsRedirect pins what a refused unit
// is told. The default text claims a user denied the call, which is false when
// an adjudicator did, and it drops the redirect the denial carried — leaving
// the unit to retry the same call or give up.
func TestUnit_DenyMessage_CarriesTheAdjudicatorsRedirect(t *testing.T) {
	ctx := context.Background()

	t.Run("a human denial keeps the plain message", func(t *testing.T) {
		h := &HITLWrapper{}
		require.Equal(t, DenyMessage, h.denyMessage(ctx, "ask-1"),
			"no reader wired means nothing is known beyond the denial")
	})

	t.Run("an adjudicator's redirect reaches the unit", func(t *testing.T) {
		h := &HITLWrapper{recorder: stubGuidance{by: "oracle", guidance: "Write to ./out/summary.txt as stated in the intent."}}
		got := h.denyMessage(ctx, "ask-2")
		require.Contains(t, got, "oracle", "the unit is told who refused it, not a fictional user")
		require.Contains(t, got, "Write to ./out/summary.txt as stated in the intent.")
		require.Contains(t, got, "Do not retry")
		require.NotContains(t, got, "User denied")
	})

	t.Run("an adjudicator with no redirect still names itself", func(t *testing.T) {
		h := &HITLWrapper{recorder: stubGuidance{by: "oracle"}}
		got := h.denyMessage(ctx, "ask-3")
		require.Contains(t, got, "oracle")
		require.NotContains(t, got, "User denied")
	})

	t.Run("a human verdict is not attributed to an agent", func(t *testing.T) {
		h := &HITLWrapper{recorder: stubGuidance{}}
		require.Equal(t, DenyMessage, h.denyMessage(ctx, "ask-4"))
	})

	t.Run("a run under a mission cites the envelope", func(t *testing.T) {
		h := &HITLWrapper{recorder: stubGuidance{by: "oracle", guidance: "Ask for the intent first."}}
		missionCtx := missiontools.WithMissionID(ctx, "m-1")
		require.Contains(t, h.denyMessage(missionCtx, "ask-5"), "per the mission envelope")
	})

	t.Run("a run with no mission cites who refused it, not an envelope it has none of", func(t *testing.T) {
		for name, rec := range map[string]stubGuidance{
			"with a note":    {by: "u_9", guidance: "Refund only up to 40 EUR."},
			"without a note": {by: "u_9"},
		} {
			t.Run(name, func(t *testing.T) {
				h := &HITLWrapper{recorder: rec}
				got := h.denyMessage(ctx, "ask-6")
				require.Contains(t, got, "Denied by u_9")
				require.NotContains(t, got, "mission envelope",
					"a webhook run belongs to no mission and has no envelope to cite")
				require.Contains(t, got, "Do not retry")
			})
		}
	})
}

type guidedApprovePolicy struct {
	stubGuidance
}

func (p guidedApprovePolicy) Evaluate(context.Context, string, string, map[string]any) (hitlservice.EvaluationResult, error) {
	return hitlservice.EvaluationResult{Action: hitlservice.ActionApprove, PolicyName: "envelope.json"}, nil
}

func TestUnit_HITLWrapper_ResumedDenialCarriesTheRecordedNote(t *testing.T) {
	policy := guidedApprovePolicy{stubGuidance{by: "u_9", guidance: "Refund only up to 40 EUR."}}
	w := NewHITLWrapper(nil, nil, policy, nil)

	ctx := context.WithValue(context.Background(), taskengine.ContextKeyToolCallID, "ask-1")
	ctx = taskengine.WithApprovalVerdicts(ctx, map[string]bool{"ask-1": false})
	res, dt, err := w.Exec(ctx, time.Now(), map[string]any{}, false,
		&taskengine.ToolsCall{Name: "billing", ToolName: "issue_refund"})

	require.NoError(t, err)
	require.Equal(t, taskengine.DataTypeString, dt)
	require.Contains(t, res, "u_9", "the resumed call must name who refused it")
	require.Contains(t, res, "Refund only up to 40 EUR.", "the note the verdict carried must reach the agent")
	require.NotContains(t, res, "User denied")
}

func TestUnit_HITLWrapper_ResumedDenialWithoutANoteKeepsThePlainMessage(t *testing.T) {
	policy := guidedApprovePolicy{}
	w := NewHITLWrapper(nil, nil, policy, nil)

	ctx := context.WithValue(context.Background(), taskengine.ContextKeyToolCallID, "ask-2")
	ctx = taskengine.WithApprovalVerdicts(ctx, map[string]bool{"ask-2": false})
	res, _, err := w.Exec(ctx, time.Now(), map[string]any{}, false,
		&taskengine.ToolsCall{Name: "billing", ToolName: "issue_refund"})

	require.NoError(t, err)
	require.Equal(t, DenyMessage, res)
}

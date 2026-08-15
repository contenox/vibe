package localtools

import (
	"context"
	"testing"

	"github.com/contenox/contenox/internal/services/hitlservice"
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
}

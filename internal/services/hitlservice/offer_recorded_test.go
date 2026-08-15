package hitlservice_test

import (
	"context"
	"testing"
	"time"

	"github.com/contenox/contenox/internal/services/hitlservice"
	"github.com/stretchr/testify/require"
)

type capturingAdjudicator struct{ seen chan hitlservice.Adjudication }

func (c *capturingAdjudicator) Adjudicate(_ context.Context, ask hitlservice.Adjudication) {
	select {
	case c.seen <- ask:
	default:
	}
}

// TestUnit_Adjudicator_OfferedFromTheRecordedPath pins the seam the in-process
// tool gate actually uses. localtools records its durable row through
// RecordPendingApproval, never RequestApproval — an offer wired only to the
// latter means a native subagent's gated call is never adjudicated at all,
// which is exactly how the feature shipped unable to run.
func TestUnit_Adjudicator_OfferedFromTheRecordedPath(t *testing.T) {
	ctx, svc, _ := adjudicationService(t)
	adj := &capturingAdjudicator{seen: make(chan hitlservice.Adjudication, 1)}
	hitlservice.SetAdjudicator(svc, adj)

	recorder, ok := svc.(hitlservice.ApprovalRecorder)
	require.True(t, ok, "the service must expose the recorder seam localtools holds")

	req := permissionAsk("m-recorded")
	require.NoError(t, recorder.RecordPendingApproval(ctx, "ask-recorded-1", req))

	select {
	case got := <-adj.seen:
		require.Equal(t, "ask-recorded-1", got.AskID)
		require.Equal(t, hitlservice.AskKindPermission, got.Kind)
		require.Equal(t, "m-recorded", got.MissionID, "an unattributed ask is one the adjudicator must decline")
		require.Equal(t, "local_fs", got.ToolsName)
		require.Equal(t, "write_file", got.ToolName)
	case <-time.After(2 * time.Second):
		t.Fatal("a recorded ask was never offered to the adjudicator")
	}
}

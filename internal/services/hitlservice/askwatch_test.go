package hitlservice_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/contenox/contenox/internal/kernel/taskengine"
	"github.com/contenox/contenox/internal/services/hitlservice"
	"github.com/contenox/contenox/internal/store/runtimetypes"
	"github.com/stretchr/testify/require"
)

type watchedAsk struct {
	row    *runtimetypes.HITLApproval
	askID  string
	reason hitlservice.AskResolution
}

type recordingWatcher struct {
	mu        sync.Mutex
	recorded  []watchedAsk
	resolved  []watchedAsk
	timeline  []string
	nilCtxHit bool
}

func (w *recordingWatcher) AskRecorded(ctx context.Context, row *runtimetypes.HITLApproval) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if ctx == nil {
		w.nilCtxHit = true
	}
	w.recorded = append(w.recorded, watchedAsk{row: row})
	w.timeline = append(w.timeline, "recorded:"+row.ID)
}

func (w *recordingWatcher) AskResolved(ctx context.Context, askID string, reason hitlservice.AskResolution) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if ctx == nil {
		w.nilCtxHit = true
	}
	w.resolved = append(w.resolved, watchedAsk{askID: askID, reason: reason})
	w.timeline = append(w.timeline, "resolved:"+askID+":"+string(reason))
}

func (w *recordingWatcher) snapshot() ([]watchedAsk, []watchedAsk, []string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]watchedAsk(nil), w.recorded...), append([]watchedAsk(nil), w.resolved...), append([]string(nil), w.timeline...)
}

func watchedService(t *testing.T) (context.Context, hitlservice.Service, runtimetypes.Store, *recordingWatcher) {
	t.Helper()
	ctx, store, _ := setupHITLDB(t)
	svc := newDurableService(t, store)
	hitlservice.SetApprovalCeiling(svc, 10*time.Second)
	w := &recordingWatcher{}
	hitlservice.SetAskWatcher(svc, w)
	return ctx, svc, store, w
}

func TestUnit_AskWatcher_SeesAGatedToolCallAndItsRetraction(t *testing.T) {
	ctx, svc, store, w := watchedService(t)
	recorder, ok := svc.(hitlservice.ApprovalRecorder)
	require.True(t, ok, "the durable gating path records through this seam")

	rule := 3
	require.NoError(t, recorder.RecordPendingApproval(ctx, "ask-perm", hitlservice.ApprovalRequest{
		ToolsName:   "billing",
		ToolName:    "issue_refund",
		Args:        map[string]any{"path": "/tmp/refund.json"},
		PolicyName:  "hitl-policy-default.json",
		MatchedRule: &rule,
		SessionID:   "cnx-sess-1",
		AgentName:   "refund-desk",
		MissionID:   "m-1",
		OnTimeout:   hitlservice.ActionDeny,
	}))

	recorded, resolved, _ := w.snapshot()
	require.Len(t, recorded, 1, "a durable ask is published as it is recorded")
	require.Empty(t, resolved)
	row := recorded[0].row
	require.Equal(t, "ask-perm", row.ID)
	require.Equal(t, "billing", row.ToolsName)
	require.Equal(t, "issue_refund", row.ToolName)
	require.Equal(t, "hitl-policy-default.json", row.PolicyName)
	require.Equal(t, "cnx-sess-1", row.SessionID)
	require.Equal(t, "refund-desk", row.AgentName)
	require.NotNil(t, row.MissionID)
	require.Equal(t, "m-1", *row.MissionID)
	require.NotNil(t, row.MatchedRule)
	require.Equal(t, 3, *row.MatchedRule)
	require.False(t, row.ExpiresAt.IsZero(), "the card needs to know how long it is answerable")

	require.NoError(t, svc.Respond(ctx, "ask-perm", true))
	_, resolved, timeline := w.snapshot()
	require.Len(t, resolved, 1)
	require.Equal(t, "ask-perm", resolved[0].askID)
	require.Equal(t, hitlservice.AskAnswered, resolved[0].reason)
	require.Equal(t, []string{"recorded:ask-perm", "resolved:ask-perm:answered"}, timeline,
		"a retraction must never overtake the publication it retracts")

	stored, err := store.GetHITLApproval(ctx, "ask-perm")
	require.NoError(t, err)
	require.Equal(t, runtimetypes.HITLApprovalApproved, stored.State)
	require.False(t, w.nilCtxHit)
}

func TestUnit_AskWatcher_SeesAnAttentionAskAndItsRetraction(t *testing.T) {
	ctx, svc, _, w := watchedService(t)

	_, err := svc.RequestAttention(ctx, hitlservice.AttentionRequest{
		Summary:   "refund 40 EUR to customer 8812?",
		Detail:    "the customer is one day outside the window",
		MissionID: "m-9",
		AgentName: "refund-desk",
		AskID:     "ask-attn",
	}, taskengine.NoopTaskEventSink{})
	var pending *hitlservice.AttentionPendingError
	require.ErrorAs(t, err, &pending, "a pre-named ask is durable and releases its caller to a checkpoint")
	require.Equal(t, "ask-attn", pending.AskID)

	recorded, _, _ := w.snapshot()
	require.Len(t, recorded, 1, "mission_ask_attention publishes on the same seam as a gated call")
	require.Equal(t, "ask-attn", recorded[0].row.ID)
	require.Equal(t, hitlservice.AttentionToolsName, recorded[0].row.ToolsName)
	require.Equal(t, hitlservice.AttentionToolName, recorded[0].row.ToolName)
	require.Equal(t, "refund 40 EUR to customer 8812?", recorded[0].row.ArgsSummary)

	require.NoError(t, svc.Answer(ctx, "ask-attn", "yes, issue it"))
	_, resolved, timeline := w.snapshot()
	require.Len(t, resolved, 1)
	require.Equal(t, hitlservice.AskAnswered, resolved[0].reason)
	require.Equal(t, []string{"recorded:ask-attn", "resolved:ask-attn:answered"}, timeline)
}

func TestUnit_AskWatcher_NamesWhyAnAskStoppedBeingAnswerable(t *testing.T) {
	for name, tc := range map[string]struct {
		close func(t *testing.T, ctx context.Context, svc hitlservice.Service, askID string)
		want  hitlservice.AskResolution
	}{
		"a verdict": {
			close: func(t *testing.T, ctx context.Context, svc hitlservice.Service, askID string) {
				require.NoError(t, svc.Respond(ctx, askID, false))
			},
			want: hitlservice.AskAnswered,
		},
		"an agent verdict": {
			close: func(t *testing.T, ctx context.Context, svc hitlservice.Service, askID string) {
				require.NoError(t, svc.RespondAsAgentBounded(ctx, askID, "oracle", true, "", 5))
			},
			want: hitlservice.AskAnswered,
		},
		"the mission going away": {
			close: func(t *testing.T, ctx context.Context, svc hitlservice.Service, askID string) {
				closed, err := svc.AbandonMissionAsks(ctx, "m-1")
				require.NoError(t, err)
				require.Equal(t, []string{askID}, closed)
			},
			want: hitlservice.AskSuperseded,
		},
	} {
		t.Run(name, func(t *testing.T) {
			ctx, svc, _, w := watchedService(t)
			recorder := svc.(hitlservice.ApprovalRecorder)
			require.NoError(t, recorder.RecordPendingApproval(ctx, "ask-1", hitlservice.ApprovalRequest{
				ToolsName: "billing",
				ToolName:  "issue_refund",
				MissionID: "m-1",
				OnTimeout: hitlservice.ActionDeny,
			}))

			tc.close(t, ctx, svc, "ask-1")

			_, resolved, _ := w.snapshot()
			require.Len(t, resolved, 1)
			require.Equal(t, "ask-1", resolved[0].askID)
			require.Equal(t, tc.want, resolved[0].reason)
		})
	}
}

func TestUnit_AskWatcher_SeesTheSweepRetractWhatNobodyAnswered(t *testing.T) {
	ctx, store, _ := setupHITLDB(t)
	svc := newDurableService(t, store)
	w := &recordingWatcher{}
	hitlservice.SetAskWatcher(svc, w)

	past := time.Now().UTC().Add(-time.Hour)
	row := seedPendingRow(t, ctx, store, string(hitlservice.ActionDeny), past, past.Add(time.Minute))

	n, err := svc.SweepExpired(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, n)

	_, resolved, _ := w.snapshot()
	require.Len(t, resolved, 1)
	require.Equal(t, row.ID, resolved[0].askID)
	require.Equal(t, hitlservice.AskExpired, resolved[0].reason)
}

func TestUnit_AskWatcher_UnmountedChangesNothing(t *testing.T) {
	ctx, store, _ := setupHITLDB(t)
	svc := newDurableService(t, store)
	hitlservice.SetApprovalCeiling(svc, 10*time.Second)

	recorder := svc.(hitlservice.ApprovalRecorder)
	require.NoError(t, recorder.RecordPendingApproval(ctx, "ask-1", hitlservice.ApprovalRequest{
		ToolsName: "billing",
		ToolName:  "issue_refund",
		OnTimeout: hitlservice.ActionDeny,
	}))
	require.NoError(t, svc.Respond(ctx, "ask-1", true))

	row, err := store.GetHITLApproval(ctx, "ask-1")
	require.NoError(t, err)
	require.Equal(t, runtimetypes.HITLApprovalApproved, row.State)

	w := &recordingWatcher{}
	hitlservice.SetAskWatcher(svc, w)
	hitlservice.SetAskWatcher(svc, nil)
	require.NoError(t, recorder.RecordPendingApproval(ctx, "ask-2", hitlservice.ApprovalRequest{
		ToolsName: "billing",
		ToolName:  "issue_refund",
		OnTimeout: hitlservice.ActionDeny,
	}))
	require.NoError(t, svc.Respond(ctx, "ask-2", true))

	recorded, resolved, _ := w.snapshot()
	require.Empty(t, recorded)
	require.Empty(t, resolved)
}

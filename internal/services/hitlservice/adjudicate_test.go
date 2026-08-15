package hitlservice_test

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/contenox/contenox/internal/kernel/taskengine"
	"github.com/contenox/contenox/internal/services/hitlservice"
	"github.com/contenox/contenox/internal/store/runtimetypes"
	libdb "github.com/contenox/contenox/libdbexec"
	"github.com/contenox/contenox/libtracker"
	"github.com/stretchr/testify/require"
)

func adjudicationService(t *testing.T) (context.Context, hitlservice.Service, runtimetypes.Store) {
	t.Helper()
	ctx := context.Background()
	db, err := libdb.NewSQLiteDBManager(ctx, filepath.Join(t.TempDir(), "adjudicate.db"), runtimetypes.SchemaSQLite)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	store := runtimetypes.New(db.WithoutTransaction())
	svc := hitlservice.NewWithDefaultPolicy(hitlservice.NewFSPolicySource(t.TempDir()), "adj-tenant", store, libtracker.NoopTracker{}, "")
	hitlservice.SetApprovalCeiling(svc, 10*time.Second)
	return ctx, svc, store
}

// scriptedAdjudicator records what it was offered and rules on it.
type scriptedAdjudicator struct {
	svc      hitlservice.Service
	approve  bool
	guidance string
	max      int

	mu   sync.Mutex
	seen []hitlservice.Adjudication
	done chan struct{}
}

func (a *scriptedAdjudicator) Adjudicate(ctx context.Context, ask hitlservice.Adjudication) {
	a.mu.Lock()
	a.seen = append(a.seen, ask)
	a.mu.Unlock()
	if ask.Kind == hitlservice.AskKindPermission {
		_ = a.svc.RespondAsAgentBounded(ctx, ask.AskID, "oracle", a.approve, a.guidance, a.max)
	}
	if a.done != nil {
		close(a.done)
	}
}

func (a *scriptedAdjudicator) offered() []hitlservice.Adjudication {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]hitlservice.Adjudication(nil), a.seen...)
}

func permissionAsk(missionID string) hitlservice.ApprovalRequest {
	return hitlservice.ApprovalRequest{
		ToolsName:  "local_fs",
		ToolName:   "write_file",
		Args:       map[string]any{"path": "/tmp/report.md"},
		PolicyName: "envelope.json",
		MissionID:  missionID,
		SessionID:  "sess-1",
		InstanceID: "inst-1",
		AgentName:  "researcher",
		OnTimeout:  hitlservice.ActionDeny,
	}
}

// TestUnit_Adjudicator_ApprovesAGatedToolCall is the whole point of the
// feature: a subagent's approve-tier call is ruled on by an agent, and the
// parked requester returns approved without a human ever seeing it.
func TestUnit_Adjudicator_ApprovesAGatedToolCall(t *testing.T) {
	ctx, svc, store := adjudicationService(t)
	adj := &scriptedAdjudicator{svc: svc, approve: true, max: 5}
	hitlservice.SetAdjudicator(svc, adj)

	approved, err := svc.RequestApproval(ctx, permissionAsk("m-1"), taskengine.NoopTaskEventSink{})
	require.NoError(t, err)
	require.True(t, approved, "the adjudicated approval must reach the parked requester")

	offered := adj.offered()
	require.Len(t, offered, 1)
	require.Equal(t, hitlservice.AskKindPermission, offered[0].Kind)
	require.Equal(t, "local_fs", offered[0].ToolsName)
	require.Equal(t, "write_file", offered[0].ToolName)
	require.Equal(t, "m-1", offered[0].MissionID)
	require.Equal(t, "researcher", offered[0].AgentName)
	require.Contains(t, offered[0].ArgsSummary, "/tmp/report.md", "the adjudicator judges the actual arguments")

	row, err := store.GetHITLApproval(ctx, offered[0].AskID)
	require.NoError(t, err)
	require.Equal(t, runtimetypes.HITLApprovalApproved, row.State)
	require.Equal(t, "oracle", hitlservice.DecidedByOf(row), "the verdict is attributed, never anonymous")
}

// TestUnit_Adjudicator_DeniesWithGuidance pins the unsticking half: a refusal
// carries a redirect, readable back off the durable row.
func TestUnit_Adjudicator_DeniesWithGuidance(t *testing.T) {
	ctx, svc, store := adjudicationService(t)
	adj := &scriptedAdjudicator{svc: svc, approve: false, guidance: "write under ./out, not /tmp", max: 5}
	hitlservice.SetAdjudicator(svc, adj)

	approved, err := svc.RequestApproval(ctx, permissionAsk("m-2"), taskengine.NoopTaskEventSink{})
	require.NoError(t, err)
	require.False(t, approved)

	row, err := store.GetHITLApproval(ctx, adj.offered()[0].AskID)
	require.NoError(t, err)
	require.Equal(t, runtimetypes.HITLApprovalDenied, row.State)
	require.Equal(t, "write under ./out, not /tmp", hitlservice.GuidanceOf(row))

	notes, err := svc.AgentGuidanceFor(ctx, "m-2")
	require.NoError(t, err)
	require.Len(t, notes, 1, "the drive loop reads the redirect back for its next turn")
	require.Equal(t, "local_fs", notes[0].ToolsName)
	require.Equal(t, "write_file", notes[0].ToolName)
	require.Equal(t, "oracle", notes[0].DecidedBy)
	require.Equal(t, "write under ./out, not /tmp", notes[0].Guidance)
}

// TestUnit_Adjudicator_BoundIsPerMissionAndSpends pins the envelope's hold on
// the whole feature: past the cap the agent cannot decide, and the ask falls
// back to whatever a human or the timeout does.
func TestUnit_Adjudicator_BoundIsPerMissionAndSpends(t *testing.T) {
	ctx, svc, store := adjudicationService(t)
	adj := &scriptedAdjudicator{svc: svc, approve: true, max: 2}
	hitlservice.SetAdjudicator(svc, adj)

	for i := 0; i < 2; i++ {
		approved, err := svc.RequestApproval(ctx, permissionAsk("m-3"), taskengine.NoopTaskEventSink{})
		require.NoError(t, err)
		require.Truef(t, approved, "call %d is inside the bound", i+1)
	}
	count, err := svc.AgentApprovalCount(ctx, "m-3")
	require.NoError(t, err)
	require.Equal(t, 2, count)

	// The third is refused by the count predicate, so the row stays pending and
	// the ceiling — not the agent — resolves it.
	hitlservice.SetApprovalCeiling(svc, 300*time.Millisecond)
	approved, err := svc.RequestApproval(ctx, permissionAsk("m-3"), taskengine.NoopTaskEventSink{})
	require.NoError(t, err)
	require.False(t, approved, "a spent bound leaves the call unapproved")

	third := adj.offered()[2]
	row, err := store.GetHITLApproval(ctx, third.AskID)
	require.NoError(t, err)
	require.Equal(t, runtimetypes.HITLApprovalPending, row.State, "the sweeper closes it out, not the agent")

	// A different mission carries its own budget.
	approvedOther, err := svc.RequestApproval(ctx, permissionAsk("m-4"), taskengine.NoopTaskEventSink{})
	require.NoError(t, err)
	require.True(t, approvedOther, "the bound is per mission, not global")
}

// TestUnit_Adjudicator_RefusesToRuleOnAQuestion pins the two contracts apart:
// approve/deny is not a verdict a question takes.
func TestUnit_Adjudicator_RefusesToRuleOnAQuestion(t *testing.T) {
	ctx, svc, store := adjudicationService(t)
	askID := "ask-question-1"
	missionID := "m-5"
	require.NoError(t, store.CreateHITLApproval(ctx, &runtimetypes.HITLApproval{
		ID:          askID,
		ToolsName:   hitlservice.AttentionToolsName,
		ToolName:    hitlservice.AttentionToolName,
		ArgsSummary: "which directory?",
		State:       runtimetypes.HITLApprovalPending,
		MissionID:   &missionID,
		CreatedAt:   time.Now().UTC(),
		ExpiresAt:   time.Now().UTC().Add(time.Hour),
	}))

	err := svc.RespondAsAgentBounded(ctx, askID, "oracle", true, "", 5)
	require.Error(t, err)
	require.Contains(t, err.Error(), "answered with words")
}

// TestUnit_Adjudicator_NoAdjudicatorChangesNothing pins the default: with none
// mounted the ask behaves exactly as it always did.
func TestUnit_Adjudicator_NoAdjudicatorChangesNothing(t *testing.T) {
	ctx, svc, _ := adjudicationService(t)
	hitlservice.SetApprovalCeiling(svc, 300*time.Millisecond)

	approved, err := svc.RequestApproval(ctx, permissionAsk("m-6"), taskengine.NoopTaskEventSink{})
	require.NoError(t, err)
	require.False(t, approved, "no adjudicator means the ceiling decides, as before")
}

// TestUnit_Adjudicator_SeesQuestionsToo pins that the one seam covers both ask
// kinds, so an oracle can answer a question and rule on a call.
func TestUnit_Adjudicator_SeesQuestionsToo(t *testing.T) {
	ctx, svc, _ := adjudicationService(t)
	adj := &scriptedAdjudicator{svc: svc, max: 5, done: make(chan struct{})}
	hitlservice.SetAdjudicator(svc, adj)
	hitlservice.SetApprovalCeiling(svc, 500*time.Millisecond)

	_, _ = svc.RequestAttention(ctx, hitlservice.AttentionRequest{
		Summary:   "which directory holds the docs?",
		Detail:    "the intent named none",
		MissionID: "m-7",
		AgentName: "researcher",
	}, taskengine.NoopTaskEventSink{})

	select {
	case <-adj.done:
	case <-time.After(2 * time.Second):
		t.Fatal("the adjudicator was never offered the question")
	}
	offered := adj.offered()
	require.Len(t, offered, 1)
	require.Equal(t, hitlservice.AskKindAttention, offered[0].Kind)
	require.Equal(t, "which directory holds the docs?", offered[0].Summary)
	require.Equal(t, "the intent named none", offered[0].Detail)
	require.Equal(t, "m-7", offered[0].MissionID)
}

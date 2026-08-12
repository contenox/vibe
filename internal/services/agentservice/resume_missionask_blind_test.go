package agentservice_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/contenox/contenox/internal/kernel/enginesvc"
	"github.com/contenox/contenox/internal/kernel/taskengine"
	"github.com/contenox/contenox/internal/services/agentservice"
	"github.com/contenox/contenox/internal/services/execservice"
	"github.com/contenox/contenox/internal/services/hitlservice"
	"github.com/contenox/contenox/internal/services/missionservice"
	"github.com/contenox/contenox/internal/services/missiontools"
	"github.com/contenox/contenox/internal/store/runtimetypes"
	libdb "github.com/contenox/contenox/libdbexec"
	"github.com/contenox/contenox/libtracker"
	"github.com/stretchr/testify/require"
)

func newAskerInstance(t *testing.T, dbPath string) *attentionInstance {
	t.Helper()
	ctx := context.Background()
	db, err := libdb.NewSQLiteDBManager(ctx, dbPath, runtimetypes.SchemaSQLite)
	require.NoError(t, err)
	store := runtimetypes.New(db.WithoutTransaction())
	hitl := hitlservice.NewWithDefaultPolicy(hitlservice.NewFSPolicySource(t.TempDir()), "blind-tenant", store, libtracker.NoopTracker{}, "")
	missions := missionservice.New(db)

	tools := missiontools.New(missions, hitlAttentionAsker{hitl: hitl}, missiontools.WithAttentionParkWindow(20*time.Millisecond))

	exec, err := taskengine.NewExec(ctx, stubModelRepo{}, tools, libtracker.NoopTracker{})
	require.NoError(t, err)
	env, err := taskengine.NewEnv(ctx, libtracker.NoopTracker{}, exec, taskengine.NewSimpleInspector(), tools)
	require.NoError(t, err)
	engine := &enginesvc.Engine{TaskService: execservice.NewTasksEnv(ctx, env, tools), Tracker: libtracker.NoopTracker{}}

	deps := agentservice.Deps{Engine: engine, DB: db, Identity: "blind"}
	inst := &attentionInstance{db: db, store: store, hitl: hitl, missions: missions, deps: deps, agent: agentservice.New(deps)}
	hitlservice.SetResumeHook(hitl, agentservice.ResumeHook(deps))
	return inst
}

func twoAskInput() taskengine.ChatHistory {
	call := func(id, summary string) taskengine.ToolCall {
		return taskengine.ToolCall{ID: id, Type: "function", Function: taskengine.FunctionCall{
			Name:      missiontools.ToolsProviderName + "." + missiontools.ToolNameAskAttention,
			Arguments: `{"summary":"` + summary + `","detail":"the intent named none"}`,
		}}
	}
	return taskengine.ChatHistory{Messages: []taskengine.Message{
		{ID: "m-user", Role: "user", Content: "do the mission", Timestamp: time.Now().UTC()},
		{ID: "m-asst", Role: "assistant", Timestamp: time.Now().UTC(), CallTools: []taskengine.ToolCall{
			call("call-ask1", "which project did you mean?"),
			call("call-ask2", "which branch should I work on?"),
		}},
	}}
}

func suspendOnFirstAsk(t *testing.T, inst *attentionInstance, missionID string) {
	t.Helper()
	ctx := missiontools.WithMissionID(context.Background(), missionID)
	resp, err := inst.agent.Prompt(ctx, agentservice.PromptRequest{
		InputValue: twoAskInput(),
		InputType:  taskengine.DataTypeChatHistory,
		Chain:      attentionChain(),
		ChainRef:   "blind-chain.json",
	})
	require.NoError(t, err)
	require.Equal(t, agentservice.StopSuspended, resp.StopReason)
	require.Equal(t, "call-ask1", resp.SuspendedApprovalID)
	_, err = inst.store.GetHITLApproval(context.Background(), "call-ask2")
	require.ErrorIs(t, err, libdb.ErrNotFound, "the second question has not been asked yet")
}

// TestSystem_Resume_SecondQuestionOnTheResumePathEngine_ReachesAHuman pins that a resumed unit's second question, on an engine that registers a real attention asker, parks and becomes an answerable ask rather than a self-answered blocker report.
func TestSystem_Resume_SecondQuestionOnTheResumePathEngine_ReachesAHuman(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "blind.db")
	ctx := context.Background()

	a := newAskerInstance(t, dbPath)
	missionID := createMission(t, a.missions)
	suspendOnFirstAsk(t, a, missionID)
	a.close()

	b := newAskerInstance(t, dbPath)
	defer b.close()
	require.NoError(t, b.hitl.Answer(ctx, "call-ask1", "the contenox runtime repo"))

	row, err := b.store.GetHITLApproval(ctx, "call-ask2")
	require.NoError(t, err, "the resumed unit's second question became a durable ask")
	require.True(t, hitlservice.IsAttentionAsk(row))
	require.Equal(t, runtimetypes.HITLApprovalPending, row.State)
	require.Equal(t, "which branch should I work on?", row.ArgsSummary)
	require.NotNil(t, row.MissionID)
	require.Equal(t, missionID, *row.MissionID, "and it is attributed to the mission whose envelope bounds it")

	_, err = b.store.GetChainCheckpoint(ctx, "call-ask2")
	require.NoError(t, err, "the run suspended on it, so something IS waiting for a human")
	_, err = b.store.GetChainCheckpoint(ctx, "call-ask1")
	require.ErrorIs(t, err, libdb.ErrNotFound, "the first checkpoint was consumed")

	reports, err := b.missions.ListReports(ctx, missionID, 10)
	require.NoError(t, err)
	for _, r := range reports {
		require.NotEqualf(t, missionservice.ReportKindBlocker, r.Kind,
			"the second question must not be downgraded to a self-answered blocker report (%q)", r.Summary)
	}

	pending, err := b.hitl.PendingAttentionAsks(ctx, missionID)
	require.NoError(t, err)
	require.Len(t, pending, 1, "an operator inspecting this mission sees exactly the open question")
	require.Equal(t, "call-ask2", pending[0].ID)
}

// TestSystem_Resume_SecondQuestionOnAWiredEngine_SuspendsAgain is the control: answering the second question on an engine wired like the dispatch path carries the run through and consumes its checkpoint.
func TestSystem_Resume_SecondQuestionOnAWiredEngine_SuspendsAgain(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "wired.db")
	ctx := context.Background()

	a := newAskerInstance(t, dbPath)
	missionID := createMission(t, a.missions)
	suspendOnFirstAsk(t, a, missionID)
	a.close()

	b := newAskerInstance(t, dbPath)
	defer b.close()
	require.NoError(t, b.hitl.Answer(ctx, "call-ask1", "the contenox runtime repo"))

	row, err := b.store.GetHITLApproval(ctx, "call-ask2")
	require.NoError(t, err, "the second question becomes its own durable ask")
	require.True(t, hitlservice.IsAttentionAsk(row))
	require.Equal(t, runtimetypes.HITLApprovalPending, row.State)
	require.Equal(t, "which branch should I work on?", row.ArgsSummary)
	_, err = b.store.GetChainCheckpoint(ctx, "call-ask2")
	require.NoError(t, err, "and the run suspends again under its key")

	require.NoError(t, b.hitl.Answer(ctx, "call-ask2", "the main branch"))
	_, err = b.store.GetChainCheckpoint(ctx, "call-ask2")
	require.ErrorIs(t, err, libdb.ErrNotFound)
}

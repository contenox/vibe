package agentservice_test

// A resumed mission run does not execute on the engine that fired it. The
// process answering the ask builds its own — contenoxcli's BuildEngine — and
// that engine must register the mission tools with a real attention asker, or
// a resumed unit's SECOND question is silently downgraded to a blocker report
// it answers itself. These tests pin what the second question costs, on an
// engine wired the way the resume path now wires it.

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/contenox/contenox/internal/kernel/enginesvc"
	"github.com/contenox/contenox/internal/kernel/taskengine"
	libdb "github.com/contenox/contenox/internal/libdbexec"
	"github.com/contenox/contenox/internal/services/agentservice"
	"github.com/contenox/contenox/internal/services/execservice"
	"github.com/contenox/contenox/internal/services/hitlservice"
	"github.com/contenox/contenox/internal/services/missionservice"
	"github.com/contenox/contenox/internal/services/missiontools"
	"github.com/contenox/contenox/internal/store/runtimetypes"
	"github.com/contenox/contenox/libtracker"
	"github.com/stretchr/testify/require"
)

// newAskerInstance is newAttentionInstance over a caller-chosen db path, with
// the mission tools wired the way both the dispatch-path engine and
// contenoxcli's resume-path engine wire them: a real attention asker over the
// same durable store. It is one constructor because the two paths must not
// differ — a resume that meets a different toolset than the run it resumes is
// the defect these tests cover.
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

// twoAskInput is one assistant turn asking two questions. The first suspends
// the run; the second is the batch's not-yet-started call that resume
// re-enters through the normal path.
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

// suspendOnFirstAsk fires the two-question run and asserts it parked on the
// first, leaving the second untouched.
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

// TestSystem_Resume_SecondQuestionOnTheResumePathEngine_ReachesAHuman is the
// flagship claim held to the resume path. Answer the first question from a
// terminal whose engine registers the mission tools the way contenoxcli's
// BuildEngine now registers them — a real attention asker over the same
// durable store — and the resumed unit's second question parks, checkpoints,
// and becomes an answerable ask an operator can see. It is NOT downgraded to a
// blocker report the unit answers itself.
func TestSystem_Resume_SecondQuestionOnTheResumePathEngine_ReachesAHuman(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "blind.db")
	ctx := context.Background()

	a := newAskerInstance(t, dbPath)
	missionID := createMission(t, a.missions)
	suspendOnFirstAsk(t, a, missionID)
	a.close()

	// The resume-path engine: a fresh process, answering an ask it never raised.
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

	// The question is a question, not a blocker the unit filed against itself.
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

// TestSystem_Resume_SecondQuestionOnAWiredEngine_SuspendsAgain is the control:
// the same run resumed on an engine wired exactly as the dispatch-path one
// closes the loop — answering the second question carries the run through from
// this process and consumes its checkpoint.
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

	// Answering it a second time carries the run through, from this process.
	require.NoError(t, b.hitl.Answer(ctx, "call-ask2", "the main branch"))
	_, err = b.store.GetChainCheckpoint(ctx, "call-ask2")
	require.ErrorIs(t, err, libdb.ErrNotFound)
}

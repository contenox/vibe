package agentservice_test

import (
	"context"
	"errors"
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

type hitlAttentionAsker struct {
	hitl hitlservice.Service
}

func (a hitlAttentionAsker) RaiseAttention(ctx context.Context, ask missiontools.AttentionAsk) (string, error) {
	answer, err := a.hitl.RequestAttention(ctx, hitlservice.AttentionRequest{
		Summary:    ask.Summary,
		Detail:     ask.Detail,
		MissionID:  ask.MissionID,
		AskID:      ask.AskID,
		ParkWindow: ask.ParkWindow,
	}, taskengine.NoopTaskEventSink{})
	var pending *hitlservice.AttentionPendingError
	if err != nil && errors.As(err, &pending) {
		return "", &taskengine.ApprovalPendingError{ApprovalID: pending.AskID, ToolName: missiontools.ToolNameAskAttention}
	}
	return answer, err
}

type attentionInstance struct {
	db       libdb.DBManager
	store    runtimetypes.Store
	hitl     hitlservice.Service
	missions missionservice.Service
	deps     agentservice.Deps
	agent    agentservice.Agent
}

func newAttentionInstance(t *testing.T, dbPath string) *attentionInstance {
	t.Helper()
	return newAttentionInstanceWithAsker(t, dbPath, func(h hitlservice.Service) missiontools.AttentionAsker {
		return hitlAttentionAsker{hitl: h}
	})
}

func newAttentionInstanceWithAsker(t *testing.T, dbPath string, newAsker func(hitlservice.Service) missiontools.AttentionAsker) *attentionInstance {
	t.Helper()
	ctx := context.Background()
	db, err := libdb.NewSQLiteDBManager(ctx, dbPath, runtimetypes.SchemaSQLite)
	require.NoError(t, err)
	store := runtimetypes.New(db.WithoutTransaction())
	hitl := hitlservice.NewWithDefaultPolicy(hitlservice.NewFSPolicySource(t.TempDir()), "e2e-tenant", store, libtracker.NoopTracker{}, "")
	missions := missionservice.New(db)

	tools := missiontools.New(missions, missiontools.WithAttentionAsker(newAsker(hitl)),
		missiontools.WithAttentionParkWindow(20*time.Millisecond))

	exec, err := taskengine.NewExec(ctx, stubModelRepo{}, tools, libtracker.NoopTracker{})
	require.NoError(t, err)
	env, err := taskengine.NewEnv(ctx, libtracker.NoopTracker{}, exec, taskengine.NewSimpleInspector(), tools)
	require.NoError(t, err)
	engine := &enginesvc.Engine{
		TaskService: execservice.NewTasksEnv(ctx, env, tools),
		Tracker:     libtracker.NoopTracker{},
	}
	deps := agentservice.Deps{Engine: engine, DB: db, Identity: "e2e"}
	inst := &attentionInstance{
		db: db, store: store, hitl: hitl, missions: missions,
		deps: deps, agent: agentservice.New(deps),
	}
	hitlservice.SetResumeHook(hitl, agentservice.ResumeHook(deps))
	return inst
}

func (i *attentionInstance) close() { _ = i.db.Close() }

func attentionChain() *taskengine.TaskChainDefinition {
	return &taskengine.TaskChainDefinition{
		ID:         "chain.attention",
		TokenLimit: 4096,
		Tasks: []taskengine.TaskDefinition{
			{
				ID:            "exec",
				Handler:       taskengine.HandleExecuteToolCalls,
				ExecuteConfig: &taskengine.LLMExecutionConfig{Model: "m", Tools: []string{missiontools.ToolsProviderName}},
				Transition: taskengine.TaskTransition{
					Branches: []taskengine.TransitionBranch{{Operator: taskengine.OpDefault, Goto: taskengine.TermEnd}},
				},
			},
		},
	}
}

func attentionInput(callID string) taskengine.ChatHistory {
	return taskengine.ChatHistory{Messages: []taskengine.Message{
		{ID: "m-user", Role: "user", Content: "do the mission", Timestamp: time.Now().UTC()},
		{ID: "m-asst", Role: "assistant", Timestamp: time.Now().UTC(), CallTools: []taskengine.ToolCall{
			{ID: callID, Type: "function", Function: taskengine.FunctionCall{
				Name:      missiontools.ToolsProviderName + "." + missiontools.ToolNameAskAttention,
				Arguments: `{"summary":"which project did you mean?","detail":"two repos match"}`,
			}},
		}},
	}}
}

func createMission(t *testing.T, missions missionservice.Service) string {
	t.Helper()
	m := &missionservice.Mission{Intent: "resolve ambiguity", AgentName: "unit", HITLPolicyName: "p.json"}
	require.NoError(t, missions.Create(context.Background(), m))
	return m.ID
}

func TestSystem_AttentionDetach_AnswerAfterRestartResumesWithOperatorsWords(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "attention-gate.db")
	ctx := context.Background()
	const callID = "call-ask1"
	const sessionID = "sess-attention"

	a := newAttentionInstance(t, dbPath)
	missionID := createMission(t, a.missions)
	createSession(t, a.db, sessionID)

	unitCtx := missiontools.WithMissionID(ctx, missionID)
	resp, err := a.agent.Prompt(unitCtx, agentservice.PromptRequest{
		SessionID:  sessionID,
		InputValue: attentionInput(callID),
		InputType:  taskengine.DataTypeChatHistory,
		Chain:      attentionChain(),
		ChainRef:   "attention-chain.json",
	})
	require.NoError(t, err, "a suspension is a typed outcome, not an error")
	require.Equal(t, agentservice.StopSuspended, resp.StopReason)
	require.Equal(t, callID, resp.SuspendedApprovalID)

	row, err := a.store.GetHITLApproval(ctx, callID)
	require.NoError(t, err)
	require.Equal(t, runtimetypes.HITLApprovalPending, row.State)
	require.True(t, hitlservice.IsAttentionAsk(row), "the durable row must read as a question")
	require.NotNil(t, row.MissionID)
	require.Equal(t, missionID, *row.MissionID)
	_, err = a.store.GetChainCheckpoint(ctx, callID)
	require.NoError(t, err)

	a.close()

	b := newAttentionInstance(t, dbPath)
	defer b.close()

	require.NoError(t, b.hitl.Answer(ctx, callID, "the contenox runtime repo, /home/x/src"),
		"Answer runs the resume synchronously via the registered hook")

	row, err = b.store.GetHITLApproval(ctx, callID)
	require.NoError(t, err)
	require.Equal(t, runtimetypes.HITLApprovalApproved, row.State)
	require.Equal(t, "the contenox runtime repo, /home/x/src", hitlservice.AnswerOf(row))
	var toolResults []string
	for _, m := range loadSessionMessages(t, b.db, sessionID) {
		if m.Role == "tool" && m.ToolCallID == callID {
			toolResults = append(toolResults, m.Content)
		}
	}
	require.Len(t, toolResults, 1, "the answer arrives exactly once")
	require.Contains(t, toolResults[0], "the contenox runtime repo, /home/x/src",
		"the operator's words ARE the tool result")

	_, err = b.store.GetChainCheckpoint(ctx, callID)
	require.ErrorIs(t, err, libdb.ErrNotFound, "a successful terminal deletes the checkpoint")

	require.ErrorIs(t, b.hitl.Answer(ctx, callID, "again"), hitlservice.ErrApprovalAlreadyResolved)
	_, err = agentservice.ResumeFromCheckpoint(ctx, b.deps, callID)
	require.ErrorIs(t, err, agentservice.ErrNoCheckpoint)
}

func TestSystem_AttentionDetach_DenyAfterRestartFilesBlockerReport(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "attention-deny.db")
	ctx := context.Background()
	const callID = "call-ask2"

	a := newAttentionInstance(t, dbPath)
	missionID := createMission(t, a.missions)
	unitCtx := missiontools.WithMissionID(ctx, missionID)

	resp, err := a.agent.Prompt(unitCtx, agentservice.PromptRequest{
		InputValue: attentionInput(callID),
		InputType:  taskengine.DataTypeChatHistory,
		Chain:      attentionChain(),
	})
	require.NoError(t, err)
	require.Equal(t, agentservice.StopSuspended, resp.StopReason)
	a.close()

	b := newAttentionInstance(t, dbPath)
	defer b.close()

	// Refused without text: the resumed unit falls back to a durable blocker.
	require.NoError(t, b.hitl.Respond(ctx, callID, false))

	reports, err := b.missions.ListReports(ctx, missionID, 10)
	require.NoError(t, err)
	require.NotEmpty(t, reports, "an unanswered resumed ask must land as a durable blocker report")
	var blocker bool
	for _, r := range reports {
		if r.Kind == missionservice.ReportKindBlocker {
			blocker = true
			require.Contains(t, r.Summary, "which project did you mean?")
		}
	}
	require.True(t, blocker, "the blocker report carries the original question")

	_, err = b.store.GetChainCheckpoint(ctx, callID)
	require.ErrorIs(t, err, libdb.ErrNotFound)
}

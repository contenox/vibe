package agentservice_test

// THE ATTENTION-DETACH GATE — the question twin of resume_e2e_test.go's S6
// gate: a mission unit asks for attention, the park window elapses with the
// operator away, the run suspends to a checkpoint and its process DIES; the
// answer, given to a completely fresh instance, resumes the chain and the
// operator's words become the tool's result — exactly once. The expiry
// variant proves the blocker fallback on resume.

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/contenox/beam/internal/kernel/enginesvc"
	"github.com/contenox/beam/internal/kernel/taskengine"
	libdb "github.com/contenox/beam/internal/libdbexec"
	"github.com/contenox/beam/internal/libtracker"
	"github.com/contenox/beam/internal/services/agentservice"
	"github.com/contenox/beam/internal/services/execservice"
	"github.com/contenox/beam/internal/services/hitlservice"
	"github.com/contenox/beam/internal/services/missionservice"
	"github.com/contenox/beam/internal/services/missiontools"
	"github.com/contenox/beam/internal/store/runtimetypes"
	"github.com/stretchr/testify/require"
)

// hitlAttentionAsker mirrors contenoxcli's missionAttentionAsker minus the bus
// announcement: forward to the real hitlservice, translate the park-window
// pending error into the engine's typed suspend error.
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
	ctx := context.Background()
	db, err := libdb.NewSQLiteDBManager(ctx, dbPath, runtimetypes.SchemaSQLite)
	require.NoError(t, err)
	store := runtimetypes.New(db.WithoutTransaction())
	hitl := hitlservice.NewWithDefaultPolicy(hitlservice.NewFSPolicySource(t.TempDir()), "e2e-tenant", store, libtracker.NoopTracker{}, "")
	missions := missionservice.New(db)

	tools := missiontools.New(missions, hitlAttentionAsker{hitl: hitl},
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

	// ── Instance A: the unit asks, nobody answers, the run suspends, A dies ─
	a := newAttentionInstance(t, dbPath)
	missionID := createMission(t, a.missions)
	createSession(t, a.db, sessionID)

	// The mission binding rides the prompt context, exactly as the unit
	// transport sets it. The 20ms park window elapses with nobody answering.
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

	// Durable state left behind: the pending QUESTION (row ID == call ID) and
	// the checkpoint under the same key.
	row, err := a.store.GetHITLApproval(ctx, callID)
	require.NoError(t, err)
	require.Equal(t, runtimetypes.HITLApprovalPending, row.State)
	require.True(t, hitlservice.IsAttentionAsk(row), "the durable row must read as a question")
	require.NotNil(t, row.MissionID)
	require.Equal(t, missionID, *row.MissionID)
	_, err = a.store.GetChainCheckpoint(ctx, callID)
	require.NoError(t, err)

	a.close()

	// ── Instance B: fresh process, the operator answers ────────────────────
	b := newAttentionInstance(t, dbPath)
	defer b.close()

	require.NoError(t, b.hitl.Answer(ctx, callID, "the contenox runtime repo, /home/x/src"),
		"Answer runs the resume synchronously via the registered hook")

	// The chain completed in B, with the operator's words as the tool result:
	// the resumed transcript's tool message carries them. THE assertion of the
	// whole slice — a vacuous "chain completed" without it would also pass
	// when the resumed call degrades to an invalid-tool result.
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

	// Answering again cannot double-run anything.
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

	// The question is REFUSED (resolved without text) — the resumed unit must
	// fall back to filing the durable blocker so the question is not lost.
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

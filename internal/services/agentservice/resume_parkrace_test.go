package agentservice_test

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/contenox/contenox/internal/kernel/taskengine"
	"github.com/contenox/contenox/internal/services/agentservice"
	"github.com/contenox/contenox/internal/services/hitlservice"
	"github.com/contenox/contenox/internal/services/missiontools"
	"github.com/contenox/contenox/internal/store/runtimetypes"
	libdb "github.com/contenox/contenox/libdbexec"
	"github.com/stretchr/testify/require"
)

type raceAnswerAsker struct {
	hitl   hitlservice.Service
	raceOn map[string]string

	mu    sync.Mutex
	raced []string
}

func (a *raceAnswerAsker) RaiseAttention(ctx context.Context, ask missiontools.AttentionAsk) (string, error) {
	answer, err := a.hitl.RequestAttention(ctx, hitlservice.AttentionRequest{
		Summary:    ask.Summary,
		Detail:     ask.Detail,
		MissionID:  ask.MissionID,
		AskID:      ask.AskID,
		ParkWindow: ask.ParkWindow,
	}, taskengine.NoopTaskEventSink{})
	var pending *hitlservice.AttentionPendingError
	if err != nil && errors.As(err, &pending) {
		if text, ok := a.raceOn[ask.Summary]; ok {
			// THE WINDOW: the waiter is already gone (RequestAttention's deferred delete) and the checkpoint can't exist yet (this call hasn't returned), so the resume hook finds nothing
			if answerErr := a.hitl.Answer(ctx, pending.AskID, text); answerErr == nil {
				a.mu.Lock()
				a.raced = append(a.raced, pending.AskID)
				a.mu.Unlock()
			}
		}
		return "", &taskengine.ApprovalPendingError{ApprovalID: pending.AskID, ToolName: missiontools.ToolNameAskAttention}
	}
	return answer, err
}

func (a *raceAnswerAsker) racedAsks() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.raced...)
}

func attentionInputPair(callA, callB string) taskengine.ChatHistory {
	return taskengine.ChatHistory{Messages: []taskengine.Message{
		{ID: "m-user", Role: "user", Content: "do the mission", Timestamp: time.Now().UTC()},
		{ID: "m-asst", Role: "assistant", Timestamp: time.Now().UTC(), CallTools: []taskengine.ToolCall{
			{ID: callA, Type: "function", Function: taskengine.FunctionCall{
				Name:      missiontools.ToolsProviderName + "." + missiontools.ToolNameAskAttention,
				Arguments: `{"summary":"first question","detail":"a"}`,
			}},
			{ID: callB, Type: "function", Function: taskengine.FunctionCall{
				Name:      missiontools.ToolsProviderName + "." + missiontools.ToolNameAskAttention,
				Arguments: `{"summary":"second question","detail":"b"}`,
			}},
		}},
	}}
}

func toolResultsFor(t *testing.T, db libdb.DBManager, sessionID, callID string) []string {
	t.Helper()
	var out []string
	for _, m := range loadSessionMessages(t, db, sessionID) {
		if m.Role == "tool" && m.ToolCallID == callID {
			out = append(out, m.Content)
		}
	}
	return out
}

// TestSystem_AttentionParkRace_AnswerInsideTheWindowStillResumesTheOriginalPrompt pins that an answer landing in the park-window/checkpoint gap still resumes the run once the checkpoint is durable, without needing the stranded-checkpoint sweep.
func TestSystem_AttentionParkRace_AnswerInsideTheWindowStillResumesTheOriginalPrompt(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "park-race-prompt.db")
	ctx := context.Background()
	const callID = "call-race-1"
	const sessionID = "sess-park-race"

	asker := &raceAnswerAsker{raceOn: map[string]string{"which project did you mean?": "the contenox runtime repo"}}
	inst := newAttentionInstanceWithAsker(t, dbPath, func(h hitlservice.Service) missiontools.AttentionAsker {
		asker.hitl = h
		return asker
	})
	defer inst.close()

	missionID := createMission(t, inst.missions)
	createSession(t, inst.db, sessionID)

	resp, err := inst.agent.Prompt(missiontools.WithMissionID(ctx, missionID), agentservice.PromptRequest{
		SessionID:  sessionID,
		InputValue: attentionInput(callID),
		InputType:  taskengine.DataTypeChatHistory,
		Chain:      attentionChain(),
		ChainRef:   "attention-chain.json",
	})
	require.NoError(t, err)
	require.Equal(t, []string{callID}, asker.racedAsks(), "the test must actually have hit the window")

	require.NotEqual(t, agentservice.StopSuspended, resp.StopReason,
		"an answered ask must not leave the run reported as suspended")

	_, err = inst.store.GetChainCheckpoint(ctx, callID)
	require.ErrorIs(t, err, libdb.ErrNotFound, "the checkpoint must be consumed, not stranded")

	results := toolResultsFor(t, inst.db, sessionID, callID)
	require.Len(t, results, 1, "the answer arrives exactly once")
	require.Contains(t, results[0], "the contenox runtime repo", "the operator's words ARE the tool result")

	stranded, err := agentservice.StrandedCheckpoints(ctx, inst.store, 10)
	require.NoError(t, err)
	require.Empty(t, stranded, "nothing may be left for the stranded-checkpoint sweep to find")
}

// TestSystem_AttentionParkRace_AnswerInsideTheWindowResumesARESUMEDRun pins the same park-window race one level deeper, where the suspension is raised by ResumeFromCheckpoint and the second question's answer lands inside the gap.
func TestSystem_AttentionParkRace_AnswerInsideTheWindowResumesARESUMEDRun(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "park-race-resume.db")
	ctx := context.Background()
	const callA, callB = "call-race-a", "call-race-b"
	const sessionID = "sess-park-race-resume"

	asker := &raceAnswerAsker{raceOn: map[string]string{"second question": "answer to the second"}}
	inst := newAttentionInstanceWithAsker(t, dbPath, func(h hitlservice.Service) missiontools.AttentionAsker {
		asker.hitl = h
		return asker
	})
	defer inst.close()

	missionID := createMission(t, inst.missions)
	createSession(t, inst.db, sessionID)

	resp, err := inst.agent.Prompt(missiontools.WithMissionID(ctx, missionID), agentservice.PromptRequest{
		SessionID:  sessionID,
		InputValue: attentionInputPair(callA, callB),
		InputType:  taskengine.DataTypeChatHistory,
		Chain:      attentionChain(),
		ChainRef:   "attention-chain.json",
	})
	require.NoError(t, err)
	require.Equal(t, agentservice.StopSuspended, resp.StopReason, "the first question parks unanswered")
	require.Equal(t, callA, resp.SuspendedApprovalID)
	require.Empty(t, asker.racedAsks(), "only the second question races the window")

	// answering the first question resumes the run, which re-enters the batch and raises the second question, whose answer lands inside the window
	require.NoError(t, inst.hitl.Answer(ctx, callA, "answer to the first"))
	require.Equal(t, []string{callB}, asker.racedAsks(), "the resumed run's ask must have hit the window")

	rowB, err := inst.store.GetHITLApproval(ctx, callB)
	require.NoError(t, err)
	require.Equal(t, runtimetypes.HITLApprovalApproved, rowB.State)

	_, err = inst.store.GetChainCheckpoint(ctx, callB)
	require.ErrorIs(t, err, libdb.ErrNotFound,
		"an answered ask whose answer landed in the park window must not leave its checkpoint stranded")

	stranded, err := agentservice.StrandedCheckpoints(ctx, inst.store, 10)
	require.NoError(t, err)
	require.Empty(t, stranded, "nothing may be left for the stranded-checkpoint sweep to find")

	resultsA := toolResultsFor(t, inst.db, sessionID, callA)
	require.Len(t, resultsA, 1, "the first answer arrives exactly once")
	require.Contains(t, resultsA[0], "answer to the first")
	resultsB := toolResultsFor(t, inst.db, sessionID, callB)
	require.Len(t, resultsB, 1, "the second answer arrives exactly once")
	require.Contains(t, resultsB[0], "answer to the second")
}

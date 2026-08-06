package agentservice_test

// The park-window/checkpoint-save race. Between an ask's park window firing
// (its waiter is deleted on the way out of RequestAttention) and the engine
// persisting the suspension checkpoint, an answer finds NEITHER: no waiter to
// wake, and no checkpoint for the resume hook to claim, so the hook's
// ErrNoCheckpoint is swallowed as the benign no-op it usually is. Milliseconds
// later the checkpoint lands with nothing left to drive it, and the answer
// looked delivered — nothing prompts the operator to run the stranded-
// checkpoint sweep that would recover it.
//
// raceAnswerAsker reproduces that window deterministically instead of racing
// wall-clock: it answers from INSIDE the tool call, which is by construction
// after the waiter is gone and before the checkpoint exists.

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/contenox/contenox/internal/kernel/taskengine"
	libdb "github.com/contenox/contenox/internal/libdbexec"
	"github.com/contenox/contenox/internal/services/agentservice"
	"github.com/contenox/contenox/internal/services/hitlservice"
	"github.com/contenox/contenox/internal/services/missiontools"
	"github.com/contenox/contenox/internal/store/runtimetypes"
	"github.com/stretchr/testify/require"
)

// raceAnswerAsker forwards to the real hitlservice and, for the asks named in
// raceOn, delivers the operator's answer inside the park-window/checkpoint
// gap before handing the typed suspend error up.
type raceAnswerAsker struct {
	hitl   hitlservice.Service
	raceOn map[string]string // ask summary -> answer text

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
			// THE WINDOW: RequestAttention's deferred delete already removed
			// the waiter, and the checkpoint cannot exist yet because this
			// tool call has not returned. The answer is recorded durably and
			// its resume hook finds nothing to resume.
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

// attentionInputPair is one assistant turn asking two questions, so the second
// one is raised by the RESUMED run rather than by the original Prompt.
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

// TestSystem_AttentionParkRace_AnswerInsideTheWindowStillResumesTheOriginalPrompt
// pins the window on the Prompt path: the answer lands with no waiter and no
// checkpoint, its resume hook is a no-op, and the run must still come back to
// life once the checkpoint is durable — never wait for an operator to run the
// stranded-checkpoint sweep.
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

// TestSystem_AttentionParkRace_AnswerInsideTheWindowResumesARESUMEDRun is the
// same window one level deeper, where the suspension is raised by
// ResumeFromCheckpoint rather than by Prompt: the first question is answered
// normally, its resume re-enters the batch, and the SECOND question's answer
// lands inside the park-window/checkpoint gap. Nothing downstream re-reads that
// ask, so before the fix its checkpoint was stranded with the answer already
// recorded — the exact shape an oracle driver reports as "the durable respond
// path resumed it" when it did not.
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

	// Answering the first question resumes the run, which re-enters the batch
	// and raises the second question — whose answer lands inside the window.
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

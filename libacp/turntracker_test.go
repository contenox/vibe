package libacp_test

import (
	"errors"
	"testing"

	"github.com/contenox/contenox/libacp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// note builds a session/update notification for one session id.
func note(sid libacp.SessionID, u libacp.SessionUpdate) libacp.SessionNotification {
	return libacp.SessionNotification{SessionID: sid, Update: u}
}

func toolCall(id string) libacp.SessionUpdate {
	return libacp.SessionUpdate{SessionUpdate: libacp.SessionUpdateToolCall, ToolCallID: id}
}

func toolCallUpdate(id string) libacp.SessionUpdate {
	return libacp.SessionUpdate{SessionUpdate: libacp.SessionUpdateToolCallUpdate, ToolCallID: id}
}

func TestUnit_TurnTracker_NotificationSequences(t *testing.T) {
	const sid = libacp.SessionID("s1")

	cases := []struct {
		name          string
		seq           []libacp.SessionUpdate
		stop          libacp.StopReason
		wantOutput    bool
		wantToolCount int
		wantErr       bool
	}{
		{
			name:       "text answer only",
			seq:        []libacp.SessionUpdate{libacp.NewAgentMessageChunk("hi"), libacp.NewAgentMessageChunk(" there")},
			stop:       libacp.StopReasonEndTurn,
			wantOutput: true,
			wantErr:    false,
		},
		{
			name:       "tool activity then final text",
			seq:        []libacp.SessionUpdate{toolCall("t1"), toolCallUpdate("t1"), libacp.NewAgentMessageChunk("done")},
			stop:       libacp.StopReasonEndTurn,
			wantOutput: true, wantToolCount: 2, wantErr: false,
		},
		{
			name:          "tool activity but no final text",
			seq:           []libacp.SessionUpdate{toolCall("t1"), toolCallUpdate("t1")},
			stop:          libacp.StopReasonEndTurn,
			wantOutput:    false,
			wantToolCount: 2,
			wantErr:       true,
		},
		{
			name:       "literally nothing",
			seq:        nil,
			stop:       libacp.StopReasonEndTurn,
			wantOutput: false, wantErr: true,
		},
		{
			name:       "only thoughts, no message",
			seq:        []libacp.SessionUpdate{libacp.NewAgentThoughtChunk("hmm"), libacp.NewAgentThoughtChunk("still thinking")},
			stop:       libacp.StopReasonEndTurn,
			wantOutput: false, wantErr: true,
		},
		{
			name:       "whitespace-only chunks are not displayable",
			seq:        []libacp.SessionUpdate{libacp.NewAgentMessageChunk("   "), libacp.NewAgentMessageChunk("\n\t")},
			stop:       libacp.StopReasonEndTurn,
			wantOutput: false, wantErr: true,
		},
		{
			name:       "non-text content counts as displayable",
			seq:        []libacp.SessionUpdate{imageChunk("data", "image/png")},
			stop:       libacp.StopReasonEndTurn,
			wantOutput: true, wantErr: false,
		},
		{
			name:       "plan and usage updates alone are not an answer",
			seq:        []libacp.SessionUpdate{planUpdate(), usageUpdate(10, 100)},
			stop:       libacp.StopReasonEndTurn,
			wantOutput: false, wantErr: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var tr libacp.TurnTracker
			for _, u := range tc.seq {
				tr.Observe(note(sid, u))
			}
			assert.Equal(t, tc.wantOutput, tr.SawDisplayableOutput())
			assert.Equal(t, tc.wantToolCount, tr.ToolUpdateCount())

			err := tr.Err(tc.stop)
			if tc.wantErr {
				require.Error(t, err)
				assert.ErrorIs(t, err, libacp.ErrNoDisplayableOutput)
				assert.True(t, libacp.IsRetryableError(err), "empty turn should be retryable")
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func TestUnit_TurnTracker_ErrEnrichesStopReasonAndToolCount(t *testing.T) {
	var tr libacp.TurnTracker
	tr.Observe(note("s1", toolCall("t1")))
	tr.Observe(note("s1", toolCallUpdate("t1")))

	err := tr.Err(libacp.StopReasonMaxTokens)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "stopReason=max_tokens")
	assert.Contains(t, err.Error(), "toolUpdates=2")
}

func TestUnit_TurnTracker_ErrUnknownStopReason(t *testing.T) {
	var tr libacp.TurnTracker
	err := tr.Err("")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "stopReason=unknown")
	assert.NotContains(t, err.Error(), "toolUpdates")
}

func TestUnit_TurnTracker_Reset(t *testing.T) {
	var tr libacp.TurnTracker
	tr.Observe(note("s1", libacp.NewAgentMessageChunk("hi")))
	tr.Observe(note("s1", toolCall("t1")))
	require.True(t, tr.SawDisplayableOutput())

	tr.Reset()
	assert.False(t, tr.SawDisplayableOutput())
	assert.Equal(t, 0, tr.ToolUpdateCount())
	assert.ErrorIs(t, tr.Err(libacp.StopReasonEndTurn), libacp.ErrNoDisplayableOutput)
}

func TestUnit_TurnTracker_ObserveIgnoresSessionID(t *testing.T) {
	// TurnTracker deliberately does not filter by session id (that is
	// FilterSessionUpdates' job); it observes whatever it is handed.
	var tr libacp.TurnTracker
	tr.Observe(note("other-session", libacp.NewAgentMessageChunk("leaked")))
	assert.True(t, tr.SawDisplayableOutput())
	assert.NoError(t, tr.Err(libacp.StopReasonEndTurn))
	assert.False(t, errors.Is(tr.Err(libacp.StopReasonEndTurn), libacp.ErrNoDisplayableOutput))
}

func imageChunk(data, mime string) libacp.SessionUpdate {
	c := libacp.NewImageContent(data, mime)
	return libacp.SessionUpdate{SessionUpdate: libacp.SessionUpdateAgentMessageChunk, Content: &c}
}

func planUpdate() libacp.SessionUpdate {
	return libacp.SessionUpdate{
		SessionUpdate: libacp.SessionUpdatePlan,
		Entries:       []libacp.PlanEntry{{Content: "step", Priority: libacp.PlanPriorityLow, Status: libacp.PlanStatusPending}},
	}
}

func usageUpdate(used, size int) libacp.SessionUpdate {
	return libacp.SessionUpdate{SessionUpdate: libacp.SessionUpdateUsageUpdate, Used: used, Size: size}
}

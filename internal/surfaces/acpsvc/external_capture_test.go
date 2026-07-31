package acpsvc

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/contenox/contenox/internal/kernel/taskengine"
	libacp "github.com/contenox/libacp"
	"github.com/stretchr/testify/require"
)

// baseTime is a fixed, deterministic turn-start stamp for capture tests.
func baseTime() time.Time { return time.Unix(1_700_000_000, 0).UTC() }

// captureFrom drives updates through externalTurnCapture's real routing
// (text/thinking coalescing, tool merge-by-id), not a hand-built slice.
func captureFrom(updates ...libacp.SessionUpdate) []externalCaptureSegment {
	c := &externalTurnCapture{}
	for _, u := range updates {
		switch u.SessionUpdate {
		case libacp.SessionUpdateAgentMessageChunk:
			if u.Content != nil {
				c.addText(u.Content.Text)
			}
		case libacp.SessionUpdateAgentThoughtChunk:
			if u.Content != nil {
				c.addThinking(u.Content.Text)
			}
		case libacp.SessionUpdateToolCall, libacp.SessionUpdateToolCallUpdate:
			c.addToolUpdate(u)
		}
	}
	return c.segments
}

func textChunk(s string) libacp.SessionUpdate  { return libacp.NewAgentMessageChunk(s) }
func thinkChunk(s string) libacp.SessionUpdate { return libacp.NewAgentThoughtChunk(s) }

// TestUnit_ExternalTurn_ToolCallsPersistAndReplay pins: a tool call round-trips
// through externalTurnMessages/externalToolReplayUpdate with fields intact.
func TestUnit_ExternalTurn_ToolCallsPersistAndReplay(t *testing.T) {
	segs := captureFrom(
		thinkChunk("let me check the file"),
		textChunk("I'll read it. "),
		libacp.SessionUpdate{
			SessionUpdate: libacp.SessionUpdateToolCall,
			ToolCallID:    "tc-1",
			Title:         "Read main.go",
			Kind:          libacp.ToolKindRead,
			Status:        libacp.ToolCallStatusPending,
			RawInput:      json.RawMessage(`{"path":"main.go"}`),
		},
		libacp.SessionUpdate{
			SessionUpdate: libacp.SessionUpdateToolCallUpdate,
			ToolCallID:    "tc-1",
			Status:        libacp.ToolCallStatusCompleted,
			RawOutput:     json.RawMessage(`"package main"`),
		},
		textChunk("Done."),
	)

	msgs := externalTurnMessages("read main.go", segs, baseTime())
	require.NotEmpty(t, msgs)

	roles := make([]string, len(msgs))
	for i, m := range msgs {
		roles[i] = m.Role
	}
	require.Equal(t, []string{"user", "assistant", "tool", "assistant"}, roles)

	require.Equal(t, "read main.go", msgs[0].Content)
	require.Equal(t, "let me check the file", msgs[1].Thinking)
	require.Equal(t, "I'll read it. ", msgs[1].Content)
	require.Equal(t, "Done.", msgs[3].Content)

	// Timestamps must strictly increase: the store sorts by added_at.
	for i := 1; i < len(msgs); i++ {
		require.True(t, msgs[i].Timestamp.After(msgs[i-1].Timestamp), "message %d not after %d", i, i-1)
	}

	tool := msgs[2]
	require.Equal(t, "tool", tool.Role)
	require.Equal(t, "tc-1", tool.ToolCallID)

	upd, ok := externalToolReplayUpdate(tool)
	require.True(t, ok)
	require.Equal(t, libacp.SessionUpdateToolCall, upd.SessionUpdate)
	require.Equal(t, "tc-1", upd.ToolCallID)
	require.Equal(t, "Read main.go", upd.Title)
	require.Equal(t, libacp.ToolKindRead, upd.Kind)
	require.Equal(t, libacp.ToolCallStatusCompleted, upd.Status)
	require.JSONEq(t, `{"path":"main.go"}`, string(upd.RawInput))
	require.JSONEq(t, `"package main"`, string(upd.RawOutput))
}

// TestUnit_ExternalTurn_ToolOnlyTurnStillPersists pins: a tool-only turn (no
// assistant text) still persists.
func TestUnit_ExternalTurn_ToolOnlyTurnStillPersists(t *testing.T) {
	segs := captureFrom(
		libacp.SessionUpdate{
			SessionUpdate: libacp.SessionUpdateToolCall,
			ToolCallID:    "tc-x",
			Title:         "bash: ls",
			Kind:          libacp.ToolKindExecute,
			Status:        libacp.ToolCallStatusCompleted,
		},
	)
	msgs := externalTurnMessages("run ls", segs, baseTime())
	require.Equal(t, []string{"user", "tool"}, rolesOf(msgs))

	upd, ok := externalToolReplayUpdate(msgs[1])
	require.True(t, ok)
	require.Equal(t, "bash: ls", upd.Title)
	require.Equal(t, libacp.ToolKindExecute, upd.Kind)
}

// TestUnit_ExternalToolReplay_IgnoresNativeToolMessage pins: a native tool
// message is never misread as an external record.
func TestUnit_ExternalToolReplay_IgnoresNativeToolMessage(t *testing.T) {
	_, ok := externalToolReplayUpdate(taskengine.Message{Role: "tool", ToolCallID: "n1", Content: "plain tool output text"})
	require.False(t, ok)
}

func rolesOf(msgs []taskengine.Message) []string {
	out := make([]string, len(msgs))
	for i, m := range msgs {
		out[i] = m.Role
	}
	return out
}

package acpsvc

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/contenox/beam/internal/kernel/taskengine"
	libacp "github.com/contenox/beam/libacp"
	"github.com/stretchr/testify/require"
)

// baseTime is a fixed, deterministic turn-start stamp for capture tests.
func baseTime() time.Time { return time.Unix(1_700_000_000, 0).UTC() }

// captureFrom drives a fresh externalTurnCapture through the same per-frame
// routing captureForHistory uses, so the test exercises the real accumulation
// (text/thinking coalescing, tool merge-by-id) rather than a hand-built slice.
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

// TestUnit_ExternalTurn_ToolCallsPersistAndReplay is the regression for the bug:
// an external agent's tool calls were only relayed live and never persisted, so
// they vanished on F5 / server restart. A captured turn must round-trip through
// externalTurnMessages (persistence) and externalToolReplayUpdate (replay) with
// the downstream's title/kind/input/output intact.
func TestUnit_ExternalTurn_ToolCallsPersistAndReplay(t *testing.T) {
	// A turn: reasoning, prose, a tool call (opened pending, completed with
	// output), then closing prose — the shape a downstream agent streams.
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

	// Ordered: user, assistant(reasoning+"I'll read it. "), tool, assistant("Done.").
	roles := make([]string, len(msgs))
	for i, m := range msgs {
		roles[i] = m.Role
	}
	require.Equal(t, []string{"user", "assistant", "tool", "assistant"}, roles)

	require.Equal(t, "read main.go", msgs[0].Content)
	require.Equal(t, "let me check the file", msgs[1].Thinking)
	require.Equal(t, "I'll read it. ", msgs[1].Content)
	require.Equal(t, "Done.", msgs[3].Content)

	// Timestamps strictly increasing so the store's added_at sort preserves order.
	for i := 1; i < len(msgs); i++ {
		require.True(t, msgs[i].Timestamp.After(msgs[i-1].Timestamp), "message %d not after %d", i, i-1)
	}

	// The tool message replays into one complete tool_call card, downstream fields intact.
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

// TestUnit_ExternalTurn_ToolOnlyTurnStillPersists guards the exact live-vs-reload
// gap: a turn that produced ONLY a tool call and no assistant text used to
// persist nothing (the old text-only capture), so the card was gone on reload.
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

// TestUnit_ExternalToolReplay_IgnoresNativeToolMessage confirms native replay is
// untouched: a native tool result (raw string Content, not an external record)
// is not misread as an external record.
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

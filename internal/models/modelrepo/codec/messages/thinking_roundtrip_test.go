package messages

import (
	"encoding/json"
	"testing"

	"github.com/contenox/beam/internal/models/modelrepo"
	"github.com/stretchr/testify/require"
)

// TestUnit_ThinkingRoundTrip_NonStreaming pins the fix for thinking+tool-use
// multi-turn: DecodeResponse persists the turn's signed thinking blocks into
// the first tool call's ProviderMeta (the one field the engine round-trips),
// and Build replays them at the head of the assistant turn — the shape
// Anthropic requires or the follow-up request 400s.
func TestUnit_ThinkingRoundTrip_NonStreaming(t *testing.T) {
	raw := []byte(`{
		"role": "assistant",
		"stop_reason": "tool_use",
		"content": [
			{"type": "thinking", "thinking": "I should list the files.", "signature": "sig-abc"},
			{"type": "redacted_thinking", "data": "opaque-blob"},
			{"type": "text", "text": "Let me check."},
			{"type": "tool_use", "id": "tu_1", "name": "fs_list", "input": {"path": "/"}}
		]
	}`)

	res, err := DecodeResponse(raw, map[string]string{"fs_list": "fs.list"})
	require.NoError(t, err)
	require.Equal(t, "I should list the files.", res.Message.Thinking)
	require.Len(t, res.ToolCalls, 1)

	meta := res.ToolCalls[0].ProviderMeta[ThinkingBlocksMetaKey]
	require.NotEmpty(t, meta, "thinking blocks must persist on the tool call's ProviderMeta")

	var blocks []map[string]any
	require.NoError(t, json.Unmarshal([]byte(meta), &blocks))
	require.Len(t, blocks, 2)
	require.Equal(t, "thinking", blocks[0]["type"])
	require.Equal(t, "sig-abc", blocks[0]["signature"], "signature must round-trip verbatim")
	require.Equal(t, "redacted_thinking", blocks[1]["type"])
	require.Equal(t, "opaque-blob", blocks[1]["data"])

	// Next turn: the assistant message (with its tool calls carrying the meta)
	// re-enters the history. Build must replay the thinking blocks FIRST.
	next := []modelrepo.Message{
		{Role: "user", Content: "list my files"},
		{Role: "assistant", Content: "Let me check.", ToolCalls: res.ToolCalls},
		{Role: "tool", ToolCallID: "tu_1", Content: "[\"a.txt\"]"},
	}
	req, _ := Build(next, &modelrepo.ChatConfig{})
	require.Len(t, req.Messages, 3)

	assistant := req.Messages[1]
	require.Equal(t, "assistant", assistant.Role)
	require.GreaterOrEqual(t, len(assistant.Content), 4)
	require.Equal(t, "thinking", assistant.Content[0].Type, "thinking blocks must precede all other blocks")
	require.Equal(t, "I should list the files.", assistant.Content[0].Thinking)
	require.Equal(t, "sig-abc", assistant.Content[0].Signature)
	require.Equal(t, "redacted_thinking", assistant.Content[1].Type)
	require.Equal(t, "opaque-blob", assistant.Content[1].Data)
	require.Equal(t, "text", assistant.Content[2].Type)
	require.Equal(t, "tool_use", assistant.Content[3].Type)

	// The wire form of a replayed thinking block must carry type/thinking/
	// signature only (no stray fields Anthropic would reject).
	wire, err := json.Marshal(assistant.Content[0])
	require.NoError(t, err)
	require.JSONEq(t, `{"type":"thinking","thinking":"I should list the files.","signature":"sig-abc"}`, string(wire))
}

// TestUnit_ThinkingRoundTrip_Streaming pins the same round-trip through the
// stream decoder: thinking_delta + signature_delta accumulate into blocks that
// ride the first tool_use ToolCallDelta's ProviderMeta.
func TestUnit_ThinkingRoundTrip_Streaming(t *testing.T) {
	d := NewStreamDecoder(nil)

	events := []string{
		`{"type":"message_start","message":{"usage":{"input_tokens":10}}}`,
		`{"type":"content_block_start","index":0,"content_block":{"type":"thinking"}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"step one"}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":" step two"}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"sig-xyz"}}`,
		`{"type":"content_block_stop","index":0}`,
		`{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"tu_9","name":"fs_list"}}`,
		`{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"path\":\"/\"}"}}`,
		`{"type":"content_block_stop","index":1}`,
		`{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":7}}`,
	}

	var toolStart *modelrepo.ToolCallDelta
	var thinking string
	for _, ev := range events {
		parcels, err := d.DecodeLine([]byte(ev))
		require.NoError(t, err)
		for _, p := range parcels {
			thinking += p.Thinking
			if p.ToolCall != nil && p.ToolCall.Name != "" {
				toolStart = p.ToolCall
			}
		}
	}

	require.Equal(t, "step one step two", thinking)
	require.NotNil(t, toolStart)
	meta := toolStart.ProviderMeta[ThinkingBlocksMetaKey]
	require.NotEmpty(t, meta, "stream decoder must attach thinking blocks to the first tool_use delta")

	var blocks []wireBlock
	require.NoError(t, json.Unmarshal([]byte(meta), &blocks))
	require.Len(t, blocks, 1)
	require.Equal(t, "step one step two", blocks[0].Thinking)
	require.Equal(t, "sig-xyz", blocks[0].Signature)
}

// TestUnit_StripThinkingBlocks: replayed blocks are removed when the outgoing
// request does not enable thinking, and emptied messages disappear.
func TestUnit_StripThinkingBlocks(t *testing.T) {
	req := Request{Messages: []wireMessage{
		{Role: "user", Content: []wireBlock{{Type: "text", Text: "hi"}}},
		{Role: "assistant", Content: []wireBlock{
			{Type: "thinking", Thinking: "t", Signature: "s"},
			{Type: "tool_use", ID: "tu_1", Name: "f", Input: json.RawMessage(`{}`)},
		}},
		{Role: "assistant", Content: []wireBlock{
			{Type: "redacted_thinking", Data: "blob"},
		}},
	}}
	StripThinkingBlocks(&req)
	require.Len(t, req.Messages, 2, "a message left empty after stripping is removed")
	require.Len(t, req.Messages[1].Content, 1)
	require.Equal(t, "tool_use", req.Messages[1].Content[0].Type)
}

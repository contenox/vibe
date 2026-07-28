package messages

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/contenox/beam/internal/models/modelrepo"
)

func TestUnit_Build_SystemExtractionAndDefaults(t *testing.T) {
	msgs := []modelrepo.Message{
		{Role: "system", Content: "be terse"},
		{Role: "user", Content: "hi"},
	}
	req, _ := Build(msgs, nil)
	if req.System != "be terse" {
		t.Fatalf("system not extracted: %q", req.System)
	}
	if req.MaxTokens != DefaultMaxTokens {
		t.Fatalf("max_tokens default not applied: %d", req.MaxTokens)
	}
	if req.Model != "" {
		t.Fatalf("codec must not set model (transport does)")
	}
	if len(req.Messages) != 1 || req.Messages[0].Role != "user" {
		t.Fatalf("system message must be lifted out of messages: %+v", req.Messages)
	}
	if len(req.Messages[0].Content) != 1 || req.Messages[0].Content[0].Type != "text" {
		t.Fatalf("user content not a text block: %+v", req.Messages[0].Content)
	}
}

func TestUnit_Build_ToolUseAndToolResultRoundTrip(t *testing.T) {
	cfg := &modelrepo.ChatConfig{
		Tools: []modelrepo.Tool{{
			Type:     "function",
			Function: &modelrepo.FunctionTool{Name: "fs.list", Description: "list files"},
		}},
	}
	msgs := []modelrepo.Message{
		{Role: "user", Content: "list /tmp"},
		{Role: "assistant", ToolCalls: []modelrepo.ToolCall{tc("toolu_1", "fs.list", `{"path":"/tmp"}`)}},
		{Role: "tool", ToolCallID: "toolu_1", Content: `{"files":["a"]}`},
	}
	req, nameMap := Build(msgs, cfg)
	if len(req.Messages) != 3 {
		t.Fatalf("expected 3 messages, got %d", len(req.Messages))
	}
	// assistant tool_use — name is sanitized on the wire
	asst := req.Messages[1]
	if asst.Role != "assistant" || len(asst.Content) != 1 || asst.Content[0].Type != "tool_use" {
		t.Fatalf("assistant tool_use block wrong: %+v", asst)
	}
	if asst.Content[0].ID != "toolu_1" || asst.Content[0].Name != "fs_list" {
		t.Fatalf("tool_use id/name wrong: %+v", asst.Content[0])
	}
	// nameMap must restore the original name
	if nameMap["fs_list"] != "fs.list" {
		t.Fatalf("nameMap missing fs.list mapping: %v", nameMap)
	}
	if string(asst.Content[0].Input) != `{"path":"/tmp"}` {
		t.Fatalf("tool_use input wrong: %s", asst.Content[0].Input)
	}
	// tool result -> user/tool_result
	res := req.Messages[2]
	if res.Role != "user" || res.Content[0].Type != "tool_result" || res.Content[0].ToolUseID != "toolu_1" {
		t.Fatalf("tool_result block wrong: %+v", res)
	}
}

func TestUnit_DecodeResponse_TextThinkingToolUse(t *testing.T) {
	raw := []byte(`{"role":"assistant","stop_reason":"tool_use","content":[
		{"type":"thinking","thinking":"hmm"},
		{"type":"text","text":"on it"},
		{"type":"tool_use","id":"toolu_9","name":"fs.list","input":{"path":"/x"}}
	]}`)
	res, err := DecodeResponse(raw, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Message.Content != "on it" {
		t.Fatalf("content: %q", res.Message.Content)
	}
	if res.Message.Thinking != "hmm" {
		t.Fatalf("thinking: %q", res.Message.Thinking)
	}
	if len(res.ToolCalls) != 1 || res.ToolCalls[0].ID != "toolu_9" || res.ToolCalls[0].Function.Name != "fs.list" {
		t.Fatalf("tool call wrong: %+v", res.ToolCalls)
	}
	// Arguments must be the JSON object serialized as a string.
	var got map[string]any
	if err := json.Unmarshal([]byte(res.ToolCalls[0].Function.Arguments), &got); err != nil {
		t.Fatalf("args not valid json: %q", res.ToolCalls[0].Function.Arguments)
	}
	if got["path"] != "/x" {
		t.Fatalf("args content: %v", got)
	}
}

// The decoder only translates the wire; assembly happens once in modelrepo.StreamAssembler.
func TestUnit_StreamDecoder_NamedEventsAndInputJSONDelta(t *testing.T) {
	d := NewStreamDecoder(nil)
	asm := modelrepo.NewStreamAssembler("anthropic", "test-model")
	lines := []string{
		`{"type":"message_start","message":{"role":"assistant","usage":{"input_tokens":21}}}`,
		`{"type":"content_block_start","index":0,"content_block":{"type":"text"}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hel"}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"lo"}}`,
		`{"type":"content_block_stop","index":0}`,
		`{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_1","name":"fs.list"}}`,
		`{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"pa"}}`,
		`{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"th\":\"/x\"}"}}`,
		`{"type":"content_block_stop","index":1}`,
		`{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":9}}`,
		`{"type":"message_stop"}`,
	}
	for _, l := range lines {
		parcels, err := d.DecodeLine([]byte(l))
		if err != nil {
			t.Fatal(err)
		}
		for _, p := range parcels {
			if err := asm.Consume(p); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := asm.Consume(d.Finish()); err != nil {
		t.Fatal(err)
	}
	res, err := asm.Result()
	if err != nil {
		t.Fatal(err)
	}
	if res.Content != "Hello" {
		t.Fatalf("assembled text: %q", res.Content)
	}
	if len(res.ToolCalls) != 1 {
		t.Fatalf("expected 1 tool call, got %d", len(res.ToolCalls))
	}
	if res.ToolCalls[0].ID != "toolu_1" || res.ToolCalls[0].Function.Name != "fs.list" {
		t.Fatalf("tool call id/name: %+v", res.ToolCalls[0])
	}
	if res.ToolCalls[0].Function.Arguments != `{"path":"/x"}` {
		t.Fatalf("assembled args: %q", res.ToolCalls[0].Function.Arguments)
	}
	if res.FinishReason != "tool_use" {
		t.Fatalf("finish reason: %q", res.FinishReason)
	}
	if res.Usage == nil || res.Usage.PromptTokens != 21 || res.Usage.CompletionTokens != 9 {
		t.Fatalf("usage: %+v", res.Usage)
	}
}

// An in-stream `error` SSE event is a hard decode error, not swallowed.
func TestUnit_StreamDecoder_ErrorEventSurfaces(t *testing.T) {
	d := NewStreamDecoder(nil)
	_, err := d.DecodeLine([]byte(`{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`))
	if err == nil {
		t.Fatal("expected an error for the in-stream error event")
	}
	if !strings.Contains(err.Error(), "overloaded_error") || !strings.Contains(err.Error(), "Overloaded") {
		t.Fatalf("error should carry the API's type and message: %v", err)
	}
}

func TestUnit_DecodeResponse_RefusalReturnsErrRefused(t *testing.T) {
	raw := []byte(`{"role":"assistant","stop_reason":"refusal","content":[]}`)
	_, err := DecodeResponse(raw, nil)
	if !errors.Is(err, modelrepo.ErrRefused) {
		t.Fatalf("expected ErrRefused, got: %v", err)
	}
}

func tc(id, name, args string) modelrepo.ToolCall {
	t := modelrepo.ToolCall{ID: id, Type: "function"}
	t.Function.Name = name
	t.Function.Arguments = args
	return t
}

package llamasession

import (
	"encoding/json"
	"testing"
)

func TestUnit_LlamaSessionTemplate_MarshalsToolHistoryForCommonChat(t *testing.T) {
	got, err := chatMessagesJSON([]chatTemplateMessage{
		{
			Role:      "assistant",
			Content:   "calling",
			ToolCalls: `[{"id":"call_1","type":"function","function":{"name":"lookup","arguments":"{}"}}]`,
		},
		{
			Role:       "tool",
			Content:    `{"answer":42}`,
			ToolCallID: "call_1",
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	var decoded []map[string]any
	if err := json.Unmarshal([]byte(got), &decoded); err != nil {
		t.Fatalf("generated invalid JSON: %v", err)
	}
	if len(decoded) != 2 {
		t.Fatalf("messages = %d, want 2: %s", len(decoded), got)
	}
	calls, ok := decoded[0]["tool_calls"].([]any)
	if !ok || len(calls) != 1 {
		t.Fatalf("tool_calls was not preserved as an array: %#v", decoded[0]["tool_calls"])
	}
	if decoded[1]["tool_call_id"] != "call_1" {
		t.Fatalf("tool_call_id = %#v, want call_1", decoded[1]["tool_call_id"])
	}
}

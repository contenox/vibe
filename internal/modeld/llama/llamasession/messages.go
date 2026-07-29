package llamasession

import "encoding/json"

type chatTemplateMessage struct {
	Role       string
	Content    string
	ToolCalls  string
	ToolCallID string
}

func chatMessagesJSON(msgs []chatTemplateMessage) (string, error) {
	type wireMsg struct {
		Role       string          `json:"role"`
		Content    string          `json:"content"`
		ToolCalls  json.RawMessage `json:"tool_calls,omitempty"`
		ToolCallID string          `json:"tool_call_id,omitempty"`
	}
	out := make([]wireMsg, len(msgs))
	for i, m := range msgs {
		wm := wireMsg{Role: m.Role, Content: m.Content, ToolCallID: m.ToolCallID}
		if m.ToolCalls != "" {
			wm.ToolCalls = json.RawMessage(m.ToolCalls)
		}
		out[i] = wm
	}
	b, err := json.Marshal(out)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

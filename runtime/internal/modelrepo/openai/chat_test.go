package openai

import (
	"encoding/json"
	"testing"
)

func TestOpenAIChatCompletionResponseReasoningContent(t *testing.T) {
	t.Parallel()
	const raw = `{
  "choices": [{
    "index": 0,
    "message": {
      "role": "assistant",
      "content": "Answer.",
      "reasoning_content": "Internal steps here."
    },
    "finish_reason": "stop"
  }]
}`
	var resp openAIChatCompletionResponse
	if err := json.Unmarshal([]byte(raw), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Choices) != 1 {
		t.Fatalf("choices: %d", len(resp.Choices))
	}
	m := resp.Choices[0].Message
	if m.Content != "Answer." || m.ReasoningContent != "Internal steps here." {
		t.Fatalf("message: content=%q reasoning_content=%q", m.Content, m.ReasoningContent)
	}
}

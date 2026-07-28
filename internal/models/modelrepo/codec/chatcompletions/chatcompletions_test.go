package chatcompletions

import (
	"encoding/base64"
	"encoding/json"
	"testing"

	"github.com/contenox/beam/internal/models/modelrepo"
)

// An image attachment becomes an OpenAI content-parts array; a text-only
// message keeps a plain-string content.
func TestUnit_Build_SerializesImageAsContentParts(t *testing.T) {
	pngBytes := []byte{0x89, 0x50, 0x4e, 0x47}
	msgs := []modelrepo.Message{
		{Role: "user", Content: "just text"},
		{Role: "user", Content: "describe this", Images: []modelrepo.ImagePart{
			{Data: pngBytes, MimeType: "image/png"},
		}},
	}

	req, _ := Build("gpt-5-mini", msgs, nil)
	raw, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var got struct {
		Messages []struct {
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, raw)
	}
	if len(got.Messages) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(got.Messages))
	}
	// Text-only message: content is a bare JSON string (unchanged wire shape).
	if b := got.Messages[0].Content; len(b) == 0 || b[0] != '"' {
		t.Errorf("text-only content should be a JSON string, got %s", b)
	}

	var parts []struct {
		Type     string `json:"type"`
		Text     string `json:"text"`
		ImageURL *struct {
			URL string `json:"url"`
		} `json:"image_url"`
	}
	if err := json.Unmarshal(got.Messages[1].Content, &parts); err != nil {
		t.Fatalf("image message content is not a parts array: %v\n%s", err, got.Messages[1].Content)
	}
	if len(parts) != 2 {
		t.Fatalf("expected [text, image_url] parts, got %d: %s", len(parts), got.Messages[1].Content)
	}
	if parts[0].Type != "text" || parts[0].Text != "describe this" {
		t.Errorf("text part wrong: %+v", parts[0])
	}
	if parts[1].Type != "image_url" || parts[1].ImageURL == nil {
		t.Fatalf("image part wrong: %+v", parts[1])
	}
	want := "data:image/png;base64," + base64.StdEncoding.EncodeToString(pngBytes)
	if parts[1].ImageURL.URL != want {
		t.Errorf("data URI mismatch:\n want %s\n  got %s", want, parts[1].ImageURL.URL)
	}
}

func TestUnit_Build_PlacesModelAndSanitizesToolNames(t *testing.T) {
	maxTok := 256
	cfg := &modelrepo.ChatConfig{
		MaxTokens: &maxTok,
		Tools: []modelrepo.Tool{{
			Type: "function",
			Function: &modelrepo.FunctionTool{
				Name:        "filesystem.list_directory",
				Description: "list a dir",
				Parameters:  map[string]any{"type": "object"},
			},
		}},
	}
	msgs := []modelrepo.Message{{Role: "user", Content: "hi"}}

	req, nameMap := Build("meta/llama-3.3-70b-instruct-maas", msgs, cfg)

	if req.Model != "meta/llama-3.3-70b-instruct-maas" {
		t.Fatalf("model not placed in body: %q", req.Model)
	}
	if req.MaxTokens == nil || *req.MaxTokens != 256 {
		t.Fatalf("max_tokens not set")
	}
	if len(req.Tools) != 1 {
		t.Fatalf("expected 1 tool, got %d", len(req.Tools))
	}
	san := req.Tools[0].Function.Name
	if san == "filesystem.list_directory" {
		t.Fatalf("tool name not sanitized (dot must be removed): %q", san)
	}
	if nameMap[san] != "filesystem.list_directory" {
		t.Fatalf("nameMap missing reverse mapping for %q: %v", san, nameMap)
	}
}

func TestUnit_Build_ToolOnlyAssistantHasNullContent(t *testing.T) {
	msgs := []modelrepo.Message{{
		Role:      "assistant",
		Content:   "",
		ToolCalls: []modelrepo.ToolCall{newToolCall("call_1", "function", "do_thing", "{}")},
	}}
	req, _ := Build("m", msgs, nil)
	b, err := json.Marshal(req.Messages[0])
	if err != nil {
		t.Fatal(err)
	}
	// content must serialize as null, not "".
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(b, &probe); err != nil {
		t.Fatal(err)
	}
	if string(probe["content"]) != "null" {
		t.Fatalf("expected null content for tool-only assistant msg, got %s", probe["content"])
	}
}

func TestUnit_DecodeResponse_TextAndToolCalls(t *testing.T) {
	// "fs_list" is the sanitized name; caller's original was "fs.list".
	nameMap := map[string]string{"fs_list": "fs.list"}
	raw := []byte(`{"choices":[{"index":0,"finish_reason":"tool_calls","message":{"role":"assistant","content":"working","tool_calls":[{"id":"call_9","type":"function","function":{"name":"fs_list","arguments":"{\"path\":\"/tmp\"}"}}]}}]}`)

	res, err := DecodeResponse(raw, nameMap)
	if err != nil {
		t.Fatal(err)
	}
	if res.Message.Content != "working" {
		t.Fatalf("content: %q", res.Message.Content)
	}
	if len(res.ToolCalls) != 1 {
		t.Fatalf("expected 1 tool call, got %d", len(res.ToolCalls))
	}
	if res.ToolCalls[0].Function.Name != "fs.list" {
		t.Fatalf("tool name not un-sanitized: %q", res.ToolCalls[0].Function.Name)
	}
	if res.ToolCalls[0].Function.Arguments != `{"path":"/tmp"}` {
		t.Fatalf("args: %q", res.ToolCalls[0].Function.Arguments)
	}
}

// The decoder only translates the wire; assembly happens once in modelrepo.StreamAssembler.
func TestUnit_StreamDecoder_EmitsRawDeltasForAssembler(t *testing.T) {
	nameMap := map[string]string{"fs_list": "fs.list"}
	d := NewStreamDecoder(nameMap)
	asm := modelrepo.NewStreamAssembler("openai", "test-model")

	lines := []string{
		`{"choices":[{"index":0,"delta":{"role":"assistant","content":"Hel"}}]}`,
		`{"choices":[{"index":0,"delta":{"content":"lo"}}]}`,
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"fs_list","arguments":"{\"pa"}}]}}]}`,
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"th\":\"/x\"}"}}]}}]}`,
		`{"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
		`{"choices":[],"usage":{"prompt_tokens":11,"completion_tokens":7,"total_tokens":18}}`,
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
	if res.ToolCalls[0].Function.Name != "fs.list" {
		t.Fatalf("stream tool name not un-sanitized: %q", res.ToolCalls[0].Function.Name)
	}
	if res.ToolCalls[0].Function.Arguments != `{"path":"/x"}` {
		t.Fatalf("assembled args: %q", res.ToolCalls[0].Function.Arguments)
	}
	if res.FinishReason != "tool_calls" {
		t.Fatalf("finish reason: %q", res.FinishReason)
	}
	if res.Usage == nil || res.Usage.PromptTokens != 11 || res.Usage.CompletionTokens != 7 || res.Usage.TotalTokens != 18 {
		t.Fatalf("usage: %+v", res.Usage)
	}
}

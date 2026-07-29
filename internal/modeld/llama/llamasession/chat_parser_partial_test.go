//go:build llamanode && llamacpp_direct

package llamasession

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/contenox/contenox/internal/modeld/llama/llamacppshim"
)

// TestUnit_ChatOutputParser_SwallowsPartialParseError is the B2 regression guard
// for the streaming-tolerance fix. It drives the parser through the seam so a
// mid-stream (partial=true) parse failure is deterministic regardless of the
// live grammar's leniency. A partial failure must be swallowed (no error, no
// delta, state preserved); the authoritative final (partial=false) parse then
// emits the full cumulative content. A final-parse failure stays fatal.
func TestUnit_ChatOutputParser_SwallowsPartialParseError(t *testing.T) {
	orig := parseChatResponse
	t.Cleanup(func() { parseChatResponse = orig })

	// Simulate llama.cpp's peg parser: reject every partial fragment as a hard
	// error (the result.end==0 throw path), succeed only on the final parse.
	partialFailures := 0
	parseChatResponse = func(input string, partial bool, _ llamacppshim.ChatSyntax, _ string, _ bool) (llamacppshim.ChatParseResult, error) {
		if partial {
			partialFailures++
			return llamacppshim.ChatParseResult{}, errors.New("llamacppshim: common chat parse: The model produced output that does not match the expected peg-native format")
		}
		return llamacppshim.ChatParseResult{Content: input}, nil
	}

	p := &chatOutputParser{reasoningFormat: "deepseek", parseToolCalls: true}

	// Two partial pushes: both must be swallowed (no error, no delta emitted).
	for i, piece := range []string{"<think>hi", "</think>the ans"} {
		text, thinking, tools, err := p.Push(piece, true)
		if err != nil {
			t.Fatalf("partial push %d returned error (B2 regression): %v", i, err)
		}
		if text != "" || thinking != "" || len(tools) != 0 {
			t.Fatalf("partial push %d emitted a delta while unparseable: text=%q thinking=%q tools=%d", i, text, thinking, len(tools))
		}
	}
	if partialFailures != 2 {
		t.Fatalf("expected 2 tolerated partial failures, got %d", partialFailures)
	}

	// Final push: the authoritative parse succeeds and the full accumulated
	// content is delivered as one cumulative delta.
	text, _, _, err := p.Push("wer", false)
	if err != nil {
		t.Fatalf("final push returned error: %v", err)
	}
	if want := "<think>hi</think>the answer"; text != want {
		t.Fatalf("final content mismatch:\n got=%q\nwant=%q", text, want)
	}
}

// TestUnit_ChatOutputParser_FinalParseErrorIsFatal confirms the complement: a
// failure on the final (partial=false) parse still aborts the turn, carrying the
// bounded diagnostics.
func TestUnit_ChatOutputParser_FinalParseErrorIsFatal(t *testing.T) {
	orig := parseChatResponse
	t.Cleanup(func() { parseChatResponse = orig })
	parseChatResponse = func(_ string, _ bool, _ llamacppshim.ChatSyntax, _ string, _ bool) (llamacppshim.ChatParseResult, error) {
		return llamacppshim.ChatParseResult{}, errors.New("common chat parse: broken")
	}
	p := &chatOutputParser{reasoningFormat: "deepseek"}
	if _, _, _, err := p.Push("garbage", false); err == nil {
		t.Fatal("expected final-parse failure to be fatal, got nil error")
	}
}

func TestUnit_ChatOutputParser_FinalParseErrorFallsBackToQwenToolCallTags(t *testing.T) {
	orig := parseChatResponse
	t.Cleanup(func() { parseChatResponse = orig })
	parseChatResponse = func(input string, _ bool, _ llamacppshim.ChatSyntax, _ string, parseTools bool) (llamacppshim.ChatParseResult, error) {
		if parseTools {
			return llamacppshim.ChatParseResult{}, errors.New("llamacppshim: common chat parse: The model produced output that does not match the expected peg-native format")
		}
		return llamacppshim.ChatParseResult{Content: input}, nil
	}

	p := &chatOutputParser{reasoningFormat: "deepseek", parseToolCalls: true}
	raw := "<tool_call>\n{\"name\":\"echo\",\"arguments\":{\"input\":\"TOOL_STRICT_OK\"}}\n</tool_call>"
	text, thinking, tools, err := p.Push(raw, false)
	if err != nil {
		t.Fatalf("final qwen tool-call fallback returned error: %v", err)
	}
	if text != "" || thinking != "" {
		t.Fatalf("fallback emitted content: text=%q thinking=%q", text, thinking)
	}
	if len(tools) != 1 {
		t.Fatalf("tool calls = %+v, want one", tools)
	}
	call := tools[0]
	if call.ID != "call_1" || call.Type != "function" || call.Function.Name != "echo" {
		t.Fatalf("normalized tool call = %+v", call)
	}
	if call.Function.Arguments != `{"input":"TOOL_STRICT_OK"}` {
		t.Fatalf("arguments = %q", call.Function.Arguments)
	}
}

// TestUnit_ChatOutputParser_RealQwenStreamReconstructs streams a real captured
// qwen3 completion (see TestSystem_LlamaChatParser_QwenThinkingStreamTolerated)
// rune-by-rune through the actual CGo parser and asserts the turn never aborts
// and the streamed content/thinking equals a single parse of the whole output.
// This guards the live streaming path against regressions on real model output.
func TestUnit_ChatOutputParser_RealQwenStreamReconstructs(t *testing.T) {
	sb, err := os.ReadFile(filepath.Join("testdata", "qwen3_chat_syntax.json"))
	if err != nil {
		t.Fatalf("read syntax fixture: %v", err)
	}
	var c struct {
		Format           int    `json:"format"`
		Parser           string `json:"parser"`
		GenerationPrompt string `json:"generation_prompt"`
		ReasoningFormat  string `json:"reasoning_format"`
		ParseToolCalls   bool   `json:"parse_tool_calls"`
	}
	if err := json.Unmarshal(sb, &c); err != nil {
		t.Fatalf("decode syntax fixture: %v", err)
	}
	rawBytes, err := os.ReadFile(filepath.Join("testdata", "qwen3_raw_completion.txt"))
	if err != nil {
		t.Fatalf("read raw fixture: %v", err)
	}
	raw := string(rawBytes)
	syntax := llamacppshim.ChatSyntax{Format: c.Format, Parser: c.Parser, GenerationPrompt: c.GenerationPrompt}

	want, err := llamacppshim.ParseChatResponse(raw, false, syntax, c.ReasoningFormat, c.ParseToolCalls)
	if err != nil {
		t.Fatalf("reference parse of complete output failed: %v", err)
	}

	p := &chatOutputParser{syntax: syntax, reasoningFormat: c.ReasoningFormat, parseToolCalls: c.ParseToolCalls}
	var gotText, gotThinking string
	runes := []rune(raw)
	for i, r := range runes {
		final := i == len(runes)-1
		text, thinking, _, perr := p.Push(string(r), !final)
		if perr != nil {
			t.Fatalf("push at rune %d (final=%v) returned error (B2 regression): %v", i, final, perr)
		}
		gotText += text
		gotThinking += thinking
	}
	if gotText != want.Content {
		t.Fatalf("streamed content mismatch:\n got=%q\nwant=%q", gotText, want.Content)
	}
	if gotThinking != want.Thinking {
		t.Fatalf("streamed thinking mismatch:\n got=%q\nwant=%q", gotThinking, want.Thinking)
	}
}

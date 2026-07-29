//go:build llamanode && llamacpp_direct

package llamasession

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/contenox/contenox/internal/modeld/llama"
)

// TestSystem_LlamaChatParser_QwenThinkingStreamTolerated drives real qwen3
// thinking + tool-calling output through the streaming chat parser and asserts
// the turn never fails on a mid-stream (partial=true) parse — the B2 live
// regression guard. The GPU-free fixtures used by chat_parser_partial_test.go
// (testdata/qwen3_chat_syntax.json, testdata/qwen3_raw_completion.txt) were
// captured from a run of this test.
//
// Requires a qwen3 GGUF via CONTENOX_LLAMA_QWEN3_GGUF (a full 4B model, so it is
// intentionally not gated by requireTinyGGUF).
func TestSystem_LlamaChatParser_QwenThinkingStreamTolerated(t *testing.T) {
	modelPath := os.Getenv("CONTENOX_LLAMA_QWEN3_GGUF")
	if modelPath == "" {
		t.Skip("set CONTENOX_LLAMA_QWEN3_GGUF to a qwen3 GGUF to run this test")
	}

	cfg := llama.Config{
		NumCtx:          2048,
		NumBatch:        256,
		NumThreads:      4,
		NumGpuLayers:    99,
		DisableBOS:      true,
		ReasoningFormat: "deepseek", // qwen3 thinking format
	}

	// A mix of pure-reasoning and tool-calling prompts increases the odds of
	// hitting the intermittent chunk-boundary state that made the peg-native
	// parser hard-fail mid-stream. Tool-call JSON fragments are the most likely
	// trigger, so include a weather tool that a prompt forces the model to call.
	weatherTools := `[{"type":"function","function":{"name":"get_weather","description":"Get the current weather for a city","parameters":{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}}}]`
	prompts := []struct {
		q     string
		tools string
	}{
		{"What is 17 + 25? Think step by step, then give the answer.", ""},
		{"A farmer has 3 pens with 4 sheep each. How many sheep? Reason first.", ""},
		{"Is 91 prime? Explain your reasoning briefly, then answer yes or no.", ""},
		{"What is the weather in Paris right now? Use the tool.", weatherTools},
		{"Check the current weather in Tokyo for me.", weatherTools},
	}

	for i, tc := range prompts {
		func() {
			sess, err := New(modelPath, cfg)
			if err != nil {
				t.Fatalf("new session: %v", err)
			}
			defer sess.Close()

			ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
			defer cancel()

			stable := "system\nYou are a helpful assistant.\n"
			suffix := "user\n" + tc.q + "\n"
			m := tinyManifest(stable, suffix)
			think := true

			if _, err := sess.EnsurePrefix(ctx, llama.PrefixInput{Text: stable, Manifest: m, Tools: tc.tools}); err != nil {
				t.Fatalf("prompt %d: ensure prefix: %v", i, err)
			}
			if _, err := sess.PrefillSuffix(ctx, llama.SuffixInput{Text: suffix, Manifest: m, EnableThinking: &think}); err != nil {
				t.Fatalf("prompt %d: prefill suffix: %v", i, err)
			}

			chunks, err := sess.Decode(ctx, llama.DecodeConfig{
				MaxTokens:       512,
				ParserProtocols: []string{llamaCommonChatReasoningParser, llamaCommonChatToolParser},
				ReasoningFormat: "deepseek",
			})
			if err != nil {
				t.Fatalf("prompt %d: decode: %v", i, err)
			}

			var text, thinking strings.Builder
			for c := range chunks {
				if c.Error != nil {
					// This is exactly the B2 failure mode: a streamed turn aborting on
					// a chat-parse error. With the fix, partial parses are tolerated and
					// only a final-parse failure is fatal.
					t.Fatalf("prompt %d: stream aborted with parse/decode error (B2 regression): %v", i, c.Error)
				}
				text.WriteString(c.Text)
				thinking.WriteString(c.Thinking)
			}
			if text.Len() == 0 && thinking.Len() == 0 {
				t.Fatalf("prompt %d: empty completion (no text and no thinking)", i)
			}
			t.Logf("prompt %d ok: thinking=%d bytes, content=%d bytes", i, thinking.Len(), text.Len())
		}()
	}
}

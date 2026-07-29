//go:build openvino && openvino_genai

package ovsession_test

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/contenox/contenox/internal/modeld/openvino/ovsession"
)

func TestSystem_OpenVINOGenAI_ReasoningStreaming(t *testing.T) {
	path := os.Getenv("CONTENOX_OPENVINO_REASONING_MODEL")
	if path == "" {
		t.Skip("set CONTENOX_OPENVINO_REASONING_MODEL to run reasoning parser tests")
	}

	session, err := ovsession.NewGenAI(path, ovsession.GenAIConfig{Device: "CPU"})
	if err != nil {
		t.Fatalf("NewGenAI: %v", err)
	}
	defer session.Close()

	ctx := context.Background()

	// deepseek-r1 model test
	messages := []ovsession.ChatMessage{
		{Role: "user", Content: "Why is the sky blue?"},
	}
	prompt, err := session.ApplyChatTemplate(messages, "")
	if err != nil {
		t.Fatalf("ApplyChatTemplate: %v", err)
	}
	t.Logf("Formatted prompt: %q", prompt)

	opts := ovsession.GenerateOptions{
		MaxNewTokens:    128,
		ParserProtocols: []string{"openvino:deepseek_r1_reasoning_incremental_parser"},
	}

	ch, err := session.Stream(ctx, prompt, opts)
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}

	var text, thinking string
	for chunk := range ch {
		if chunk.Error != nil {
			t.Fatalf("Chunk error: %v", chunk.Error)
		}
		t.Logf("Chunk: text=%q thinking=%q", chunk.Text, chunk.Thinking)
		text += chunk.Text
		thinking += chunk.Thinking
	}

	if thinking == "" {
		t.Fatal("expected reasoning parser to emit thinking content")
	}
	if strings.Contains(text, "<think>") || strings.Contains(text, "Okay, so I'm trying") {
		t.Fatalf("reasoning leaked into visible text: %q", text)
	}

	t.Logf("Visible streamed output: %s", text)
	t.Logf("Thinking streamed output: %s", thinking)
}

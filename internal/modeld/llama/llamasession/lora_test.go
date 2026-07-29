//go:build llamanode

package llamasession

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/contenox/contenox/internal/modeld/llama"
)

func TestSystem_LlamaSessionLoRA_AdapterChangesContinuation(t *testing.T) {
	modelPath := os.Getenv("CONTENOX_LLAMA_LORA_GGUF")
	if modelPath == "" {
		t.Skip("set CONTENOX_LLAMA_LORA_GGUF to a base GGUF model with a matching adapter")
	}
	adapterPath := os.Getenv("CONTENOX_LLAMA_LORA_ADAPTER")
	if adapterPath == "" {
		t.Skip("set CONTENOX_LLAMA_LORA_ADAPTER to a GGUF LoRA adapter for the model")
	}

	cfg := llama.Config{NumCtx: 128, NumBatch: 32, NumThreads: 2, DisableBOS: true}
	stable := "system\n"
	suffix := "def add(a, b):\n"

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// Base model continuation.
	base, err := New(modelPath, cfg)
	if err != nil {
		t.Fatalf("New base: %v", err)
	}
	baseOut, err := decodeContinuation(ctx, base, stable, suffix, 12)
	if err != nil {
		t.Fatalf("decode base: %v", err)
	}
	if err := base.Close(); err != nil {
		t.Fatalf("close base: %v", err)
	}

	// Same model + LoRA adapter applied at the session context.
	adapted, err := NewWithAdapters(modelPath, cfg, []llama.AdapterSpec{
		{Name: "smoke-lora", Path: adapterPath, Scale: 8.0},
	})
	if err != nil {
		t.Fatalf("NewWithAdapters: %v", err)
	}
	defer adapted.Close()
	loraOut, err := decodeContinuation(ctx, adapted, stable, suffix, 12)
	if err != nil {
		t.Fatalf("decode lora: %v", err)
	}
	if strings.TrimSpace(loraOut) == "" {
		t.Fatal("LoRA-adapted session produced empty output")
	}

	changed := baseOut != loraOut
	t.Logf("base=%q", baseOut)
	t.Logf("lora=%q", loraOut)
	t.Logf("adapter changed continuation: %v", changed)

	if os.Getenv("CONTENOX_LLAMA_LORA_EXPECT_DIFF") == "1" && !changed {
		t.Fatal("expected the LoRA adapter to change the continuation")
	}
}

// decodeContinuation prefills stable+suffix and greedily decodes maxTokens.
func decodeContinuation(ctx context.Context, sess llama.Session, stable, suffix string, maxTokens int) (string, error) {
	manifest := tinyManifest(stable, suffix)
	if _, err := sess.EnsurePrefix(ctx, llama.PrefixInput{Text: stable, Manifest: manifest}); err != nil {
		return "", err
	}
	if _, err := sess.PrefillSuffix(ctx, llama.SuffixInput{Text: suffix, Manifest: manifest}); err != nil {
		return "", err
	}
	temp := 0.0
	seed := 7
	chunks, err := sess.Decode(ctx, llama.DecodeConfig{MaxTokens: maxTokens, Temperature: &temp, Seed: &seed})
	if err != nil {
		return "", err
	}
	var out strings.Builder
	for chunk := range chunks {
		if chunk.Error != nil {
			return "", chunk.Error
		}
		out.WriteString(chunk.Text)
	}
	return out.String(), nil
}

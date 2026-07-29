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

func TestSystem_LlamaGPU_Throughput(t *testing.T) {
	modelPath := os.Getenv("CONTENOX_LLAMA_TINY_GGUF")
	requireTinyGGUF(t, modelPath)

	sess, err := New(modelPath, llama.Config{
		NumCtx: 1024, NumBatch: 256, NumThreads: 1, NumGpuLayers: 99, DisableBOS: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	stable := "system\n" + strings.Repeat("You are a precise coding assistant. ", 60) + "\n"
	suffix := "user\nSummarize the instructions above in one sentence.\n"
	m := tinyManifest(stable, suffix)

	t0 := time.Now()
	pfx, err := sess.EnsurePrefix(ctx, llama.PrefixInput{Text: stable, Manifest: m})
	if err != nil {
		t.Fatal(err)
	}
	prefillDur := time.Since(t0)
	if _, err := sess.PrefillSuffix(ctx, llama.SuffixInput{Text: suffix, Manifest: m}); err != nil {
		t.Fatal(err)
	}

	const decodeN = 96
	t1 := time.Now()
	chunks, err := sess.Decode(ctx, llama.DecodeConfig{MaxTokens: decodeN})
	if err != nil {
		t.Fatal(err)
	}
	produced := 0
	for c := range chunks {
		if c.Error != nil {
			t.Fatalf("decode error: %v", c.Error)
		}
		if c.Text != "" {
			produced++
		}
	}
	decodeDur := time.Since(t1)

	if pfx.PrefixTokens == 0 {
		t.Fatal("no prefill tokens")
	}
	prefillTps := float64(pfx.PrefixTokens) / prefillDur.Seconds()
	decodeTps := float64(produced) / decodeDur.Seconds()
	t.Logf("GPU throughput: prefill %d tok in %v (%.0f tok/s) | decode %d tok in %v (%.0f tok/s)",
		pfx.PrefixTokens, prefillDur.Round(time.Millisecond), prefillTps,
		produced, decodeDur.Round(time.Millisecond), decodeTps)
}

//go:build openvino && openvino_genai

package ovsession

import (
	"context"
	"os"
	"testing"
	"time"
)

// TestSystem_GenAICacheEviction_GeneratesWithNativeEviction proves OpenVINO's
// native KV cache eviction (sink + recent + evictable middle) runs through our
// shim end-to-end: a session built with use_cache_eviction generates on a real
// IR model. This is the OpenVINO expression of the residency policy — declarative
// config, not runtime KV surgery.
func TestSystem_GenAICacheEviction_GeneratesWithNativeEviction(t *testing.T) {
	modelDir := os.Getenv("CONTENOX_OPENVINO_TEST_MODEL")
	if modelDir == "" {
		t.Skip("set CONTENOX_OPENVINO_TEST_MODEL to an OpenVINO IR directory")
	}
	on := true
	sess, err := NewGenAI(modelDir, GenAIConfig{
		Device:               os.Getenv("CONTENOX_OPENVINO_TEST_DEVICE"),
		UseCacheEviction:     &on,
		CacheEvictStartSize:  32,  // sinks
		CacheEvictRecentSize: 128, // recent window
		CacheEvictMaxSize:    256, // budget; evictable = 256 - 32 - 128
	})
	if err != nil {
		t.Fatalf("NewGenAI with cache eviction: %v", err)
	}
	defer sess.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	res, err := sess.Generate(ctx, "def add(a, b):", GenerateOptions{MaxNewTokens: 16})
	if err != nil {
		t.Fatalf("Generate with native cache eviction: %v", err)
	}
	if res.Text == "" {
		t.Fatal("empty generation with cache eviction enabled")
	}
	t.Logf("native cache-eviction generation: %q", res.Text)
}

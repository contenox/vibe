//go:build openvino && openvino_genai

package ovsession

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestSystem_OpenVINOGenAI_PrefixReuseWarmsPrefill checks that OpenVINO's
// ContinuousBatchingPipeline prefix cache makes a repeated stable prefix cheaper
// to process.
//
// One session, prefix caching on (the NewGenAI default). We send a large stable
// "repo context" prefix with two different tiny suffixes and ask for one token
// each. The first call pays full prefill; the second should reuse the cached KV
// of the shared prefix and be materially faster. If this fails, automatic prefix
// caching is not delivering warm reuse on this stack.
func TestSystem_OpenVINOGenAI_PrefixReuseWarmsPrefill(t *testing.T) {
	modelDir := os.Getenv("CONTENOX_OPENVINO_TEST_MODEL")
	if modelDir == "" {
		t.Skip("set CONTENOX_OPENVINO_TEST_MODEL to an OpenVINO IR model directory")
	}
	device := os.Getenv("CONTENOX_OPENVINO_TEST_DEVICE")
	if device == "" {
		device = "CPU"
	}

	s, err := NewGenAI(modelDir, GenAIConfig{Device: device})
	require.NoError(t, err)
	defer s.Close()

	// A large, stable prefix — the kind of repo/tool/system context a coding node
	// keeps hot across turns. Big enough that prefill cost dominates fixed
	// per-request overhead.
	var b strings.Builder
	for i := 0; i < 2000; i++ {
		fmt.Fprintf(&b, "// file pkg/mod%04d.go: func Handler%04d(ctx context.Context) error { return nil }\n", i, i)
	}
	prefix := b.String()

	gen := func(prompt string) (time.Duration, GenAIResult) {
		t.Helper()
		start := time.Now()
		res, err := s.Generate(context.Background(), prompt, GenerateOptions{MaxNewTokens: 1})
		require.NoError(t, err)
		return time.Since(start), res
	}

	// Warm the runtime with an unrelated short prompt so one-time init is not
	// attributed to the cold measurement below.
	_, _ = gen("hello")

	cold, coldRes := gen(prefix + "\n// QUESTION A: name one function.\n")
	warm, warmRes := gen(prefix + "\n// QUESTION B: name another function.\n")

	t.Logf("prefix bytes = %d", len(prefix))
	t.Logf("cold (full prefill + 1 tok) = %v  cache_usage=%.4f max=%.4f size_bytes=%d",
		cold, coldRes.Metrics.CacheUsage, coldRes.Metrics.MaxCacheUsage, coldRes.Metrics.CacheSizeInBytes)
	t.Logf("warm (cached prefix + 1 tok) = %v  cache_usage=%.4f max=%.4f",
		warm, warmRes.Metrics.CacheUsage, warmRes.Metrics.MaxCacheUsage)
	if cold > 0 {
		t.Logf("warm/cold = %.2f  (speedup %.1f%%)",
			float64(warm)/float64(cold), (1-float64(warm)/float64(cold))*100)
	}

	require.Less(t, warm, cold, "warm prefix run must be faster than cold")
	require.Less(t, float64(warm), 0.8*float64(cold),
		"expected >=20%% speedup from prefix cache reuse; got cold=%v warm=%v", cold, warm)
}

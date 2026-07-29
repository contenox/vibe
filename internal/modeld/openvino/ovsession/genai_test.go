//go:build openvino && openvino_genai

package ovsession

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestSystem_OpenVINOGenAI_SessionGenerateAndClose(t *testing.T) {
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

	got, err := s.Generate(context.Background(), "def add(a, b):", GenerateOptions{MaxNewTokens: 8})
	require.NoError(t, err)
	require.NotEmpty(t, got.Text)
	require.Equal(t, uint64(1), got.Metrics.Requests)
	require.Equal(t, uint64(1), got.Metrics.ScheduledRequests)
	require.Greater(t, got.Metrics.CacheSizeInBytes, uint64(0))

	require.NoError(t, s.Close())
	require.NoError(t, s.Close())

	_, err = s.Generate(context.Background(), "def sub(a, b):", GenerateOptions{MaxNewTokens: 1})
	require.ErrorContains(t, err, "closed")
}

func TestSystem_OpenVINOGenAI_ColdKVCapability(t *testing.T) {
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

	require.True(t, s.SupportsColdKV(), "GenAI bridge should expose cold KV import/export hooks")
}

func TestSystem_OpenVINOGenAI_TokenPrefillAndGenerate(t *testing.T) {
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

	ctx := context.Background()
	tokens, err := s.Tokenize(ctx, "def add(a, b):", true)
	require.NoError(t, err)
	require.NotEmpty(t, tokens)

	require.NoError(t, s.PrefillTokens(ctx, tokens))
	got, err := s.GenerateTokens(ctx, tokens, GenerateOptions{MaxNewTokens: 8})
	require.NoError(t, err)
	require.NotEmpty(t, got.Text)
}

func TestSystem_OpenVINOGenAI_ShiftedColdKVImport(t *testing.T) {
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

	ctx := context.Background()
	tokens, err := s.Tokenize(ctx, "def add(a, b):\n    return a + b\n\nprint(add(2, 3))\n", true)
	require.NoError(t, err)
	require.GreaterOrEqual(t, len(tokens), 8)

	start, end := 2, 5
	tokenHash := "shifted-cold-kv-system"
	require.NoError(t, s.PrefillTokens(ctx, tokens))
	kv, err := s.ExportColdKV(ctx, ColdKVRange{
		Start:        start,
		End:          end,
		Tokens:       append([]int(nil), tokens[start:end]...),
		PrefixTokens: append([]int(nil), tokens...),
		TokenHash:    tokenHash,
	})
	require.NoError(t, err)
	require.NotEmpty(t, kv)

	resident := append(append([]int(nil), tokens[:start]...), tokens[end:]...)
	require.NoError(t, s.PrefillTokens(ctx, resident))
	dest := append(append([]int(nil), resident...), tokens[start:end]...)
	require.NoError(t, s.ImportColdKV(ctx, ColdKVRange{
		Start:        start,
		End:          end,
		DestStart:    len(resident),
		Tokens:       append([]int(nil), tokens[start:end]...),
		PrefixTokens: dest,
		TokenHash:    tokenHash,
	}, kv))

	got, err := s.GenerateTokens(ctx, dest, GenerateOptions{MaxNewTokens: 1})
	require.NoError(t, err)
	require.Equal(t, uint64(1), got.Metrics.Requests)
}

func TestSystem_OpenVINOGenAI_ContextCanceledBeforeGenerate(t *testing.T) {
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

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = s.Generate(ctx, "def add(a, b):", GenerateOptions{MaxNewTokens: 8})
	require.ErrorIs(t, err, context.Canceled)
}

// TestSystem_OpenVINOGenAI_StreamCanceledInFlight cancels generation *after* it is
// demonstrably underway (we have already received decoded tokens) and asserts the
// native cancel hook stops the stream early with context.Canceled, rather than
// running the full token budget. This is the deterministic in-flight counterpart
// to the pre-canceled-context case above.
func TestSystem_OpenVINOGenAI_StreamCanceledInFlight(t *testing.T) {
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

	const budget = 1024
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch, err := s.Stream(ctx, "Write a very long Go program:", GenerateOptions{MaxNewTokens: budget})
	require.NoError(t, err)

	var got int
	var sawCancel bool
	for chunk := range ch {
		if chunk.Error != nil {
			require.ErrorIs(t, chunk.Error, context.Canceled)
			sawCancel = true
			continue // keep draining until the channel closes
		}
		got++
		if got == 2 {
			cancel() // generation is in-flight; cut it off
		}
	}

	require.True(t, sawCancel, "expected an in-flight context.Canceled stream error")
	require.Greater(t, got, 0, "expected some tokens before cancellation")
	require.Less(t, got, budget, "stream should stop well before the full token budget")
}

// TestSystem_OpenVINOGenAI_CancelRaceStress hammers the cancel paths to expose
// B8: a native cancel-flag reset race where a cancel issued around dispatch can
// be clobbered by cx_genai_generate resetting cancel_requested at generation
// start, so the call runs the full budget (a hang/timeout) instead of returning
// promptly. Each iteration is wrapped in a watchdog so a stuck call fails fast
// rather than hanging the suite.
func TestSystem_OpenVINOGenAI_CancelRaceStress(t *testing.T) {
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

	// watchdog runs fn and fails the test if it does not finish in time — a hang
	// (full-budget run after a lost cancel) is the B8 symptom we are hunting.
	watchdog := func(t *testing.T, label string, budget int, fn func()) {
		t.Helper()
		done := make(chan struct{})
		go func() { defer close(done); fn() }()
		// Generous ceiling: a properly-cancelled call returns in well under this;
		// only a lost-cancel full-budget run (or deadlock) overruns it.
		select {
		case <-done:
		case <-time.After(20 * time.Second):
			t.Fatalf("%s: call did not return within watchdog window (B8: cancel likely lost, ran full budget=%d)", label, budget)
		}
	}

	const iters = 200
	const budget = 4096

	for i := 0; i < iters; i++ {
		// (a) Pre-canceled context: must return context.Canceled promptly.
		watchdog(t, "pre-cancel-generate", budget, func() {
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			_, gerr := s.Generate(ctx, "def add(a, b):", GenerateOptions{MaxNewTokens: budget})
			require.ErrorIs(t, gerr, context.Canceled)
		})

		// (b) Cancel fired concurrently right around dispatch: must stop well
		// under budget with context.Canceled, never run the full budget.
		watchdog(t, "race-cancel-stream", budget, func() {
			ctx, cancel := context.WithCancel(context.Background())
			ch, serr := s.Stream(ctx, "Write a very long Go program:", GenerateOptions{MaxNewTokens: budget})
			require.NoError(t, serr)
			// Cancel almost immediately to race the generation start.
			go cancel()
			produced := 0
			for chunk := range ch {
				if chunk.Error != nil {
					require.ErrorIs(t, chunk.Error, context.Canceled)
					continue
				}
				produced++
			}
			require.Less(t, produced, budget, "stream ran the full budget despite an early cancel (B8)")
			cancel()
		})
	}
}

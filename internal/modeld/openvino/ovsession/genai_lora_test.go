//go:build openvino && openvino_genai

package ovsession

import (
	"context"
	"os"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestSystem_OpenVINOGenAI_LoRAAdapterGenerates is the low-level smoke test for
// the dynamic LoRA plumbing: it constructs a ContinuousBatchingPipeline with a
// LoRA adapter registered via the ov::genai::adapters property (MODE_DYNAMIC) and
// confirms the session generates. It hard-asserts construction + generation — the
// parity gate: the CB pipeline (not just LLMPipeline) honors the adapters property
// — and logs whether the adapter changed the greedy continuation.
//
// Verified findings behind this test (OpenVINO GenAI 2026.2):
//   - MODE_DYNAMIC LoRA DOES apply to int4 (u4) weight-compressed models: the
//     dynamic transform inserts on activations, not weights. (MODE_FUSE is the one
//     that rejects low-bit weights with "Use f32/f16/bf16 weights only".)
//   - Adapter tensor names must be canonical PEFT
//     ("base_model.model.model.layers.N.self_attn.q_proj.lora_A.weight"); shorter
//     names match zero nodes and OpenVINO logs "unused LoRA tensors".
//   - testdata/make_lora.py generates a name-matched synthetic Qwen2-0.5B adapter.
//
// Set CONTENOX_OPENVINO_TEST_LORA_EXPECT_DIFF=1 to hard-assert the adapter changed
// the continuation (requires a name-matched adapter for the model under test).
func TestSystem_OpenVINOGenAI_LoRAAdapterGenerates(t *testing.T) {
	modelDir := os.Getenv("CONTENOX_OPENVINO_TEST_MODEL")
	if modelDir == "" {
		t.Skip("set CONTENOX_OPENVINO_TEST_MODEL to an OpenVINO IR model directory")
	}
	loraPath := os.Getenv("CONTENOX_OPENVINO_TEST_LORA")
	if loraPath == "" {
		t.Skip("set CONTENOX_OPENVINO_TEST_LORA to a safetensors LoRA adapter for the model")
	}
	device := os.Getenv("CONTENOX_OPENVINO_TEST_DEVICE")
	if device == "" {
		device = "CPU"
	}

	const prompt = "def add(a, b):"
	const maxTokens = 16

	// Base model continuation, for comparison.
	base, err := NewGenAI(modelDir, GenAIConfig{Device: device})
	require.NoError(t, err)
	baseRes, err := base.Generate(context.Background(), prompt, GenerateOptions{MaxNewTokens: maxTokens})
	require.NoError(t, err)
	require.NoError(t, base.Close())

	// Same model + dynamic LoRA adapter registered at construction.
	s, err := NewGenAI(modelDir, GenAIConfig{
		Device: device,
		LoRAAdapters: []GenAILoRAAdapter{
			{Path: loraPath, Alpha: 4.0},
		},
	})
	require.NoError(t, err, "constructing CB pipeline with LoRA adapter registered")
	defer func() { require.NoError(t, s.Close()) }()

	res, err := s.Generate(context.Background(), prompt, GenerateOptions{MaxNewTokens: maxTokens})
	require.NoError(t, err, "generating with LoRA adapter active")
	require.NotEmpty(t, res.Text, "LoRA-adapted session produced empty output")

	changed := baseRes.Text != res.Text
	t.Logf("base=%q", baseRes.Text)
	t.Logf("lora=%q", res.Text)
	t.Logf("adapter changed output: %v", changed)

	if os.Getenv("CONTENOX_OPENVINO_TEST_LORA_EXPECT_DIFF") == "1" {
		require.True(t, changed, "expected the LoRA adapter to change the continuation (set EXPECT_DIFF only for fp16/bf16 models)")
	}
}

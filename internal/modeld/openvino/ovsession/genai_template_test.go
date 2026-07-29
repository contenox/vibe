//go:build openvino && openvino_genai

package ovsession

import (
	"os"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestSystem_OpenVINOGenAI_ApplyChatTemplate checks that the provider feeds the
// model its own chat template from tokenizer_config.json rather than a
// hand-rolled ChatML string. Qwen2.5 uses <|im_start|> role markers, so the
// rendered prompt must contain those and must not be the <|system|> fallback
// layout.
func TestSystem_OpenVINOGenAI_ApplyChatTemplate(t *testing.T) {
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

	out, err := s.ApplyChatTemplate([]ChatMessage{
		{Role: "system", Content: "You are a terse coding assistant."},
		{Role: "user", Content: "say hi"},
	}, "")
	require.NoError(t, err)
	require.NotEmpty(t, out)
	require.Contains(t, out, "<|im_start|>", "expected the model's own ChatML template markers")
	require.Contains(t, out, "You are a terse coding assistant.")
	require.Contains(t, out, "say hi")
	require.NotContains(t, out, "<|system|>", "must not be the hand-rolled fallback format")

	t.Logf("templated prompt:\n%s", out)
}

func TestSystem_OpenVINOGenAI_ProbeChatTemplate(t *testing.T) {
	modelDir := os.Getenv("CONTENOX_OPENVINO_TEST_MODEL")
	if modelDir == "" {
		t.Skip("set CONTENOX_OPENVINO_TEST_MODEL to an OpenVINO IR model directory")
	}

	probe, err := ProbeChatTemplate(modelDir)
	require.NoError(t, err)
	require.Equal(t, "openvino:minja", probe.FormatName)

	t.Logf("chat template probe: %+v", probe)
}

// TestSystem_OpenVINOGenAI_ApplyChatTemplateWithTools checks that tool
// definitions are rendered into the prompt via the model's own template
// tools-handling.
func TestSystem_OpenVINOGenAI_ApplyChatTemplateWithTools(t *testing.T) {
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

	toolsJSON := `[{"type":"function","function":{"name":"get_weather","description":"Get the weather for a city","parameters":{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}}}]`
	out, err := s.ApplyChatTemplate([]ChatMessage{
		{Role: "user", Content: "what is the weather in SF?"},
	}, toolsJSON)
	require.NoError(t, err)
	require.NotEmpty(t, out)
	require.Contains(t, out, "get_weather", "the tool definition must be rendered into the prompt")

	t.Logf("templated prompt with tools:\n%s", out)
}

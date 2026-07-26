package contenoxcli

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestUnit_BuildTemplateVars_PreservesConfiguredDefaultForRecovery(t *testing.T) {
	vars := buildTemplateVars(chatOpts{
		EffectiveDefaultModel:       "does-not-exist",
		EffectiveDefaultProvider:    "vllm",
		EffectiveConfiguredModel:    "qwen3-4b",
		EffectiveConfiguredProvider: "anthropic",
		EffectiveAltDefaultModel:    "small-model",
		EffectiveAltDefaultProvider: "ollama",
		EffectiveMaxTokens:          "4096",
		EffectiveThink:              "medium",
	})

	require.Equal(t, "does-not-exist", vars["model"])
	require.Equal(t, "vllm", vars["provider"])
	require.Equal(t, "qwen3-4b", vars["default_model"])
	require.Equal(t, "anthropic", vars["default_provider"])
	require.Equal(t, "small-model", vars["alt_model"])
	require.Equal(t, "ollama", vars["alt_provider"])
	require.Equal(t, "4096", vars["max_tokens"])
	require.Equal(t, "medium", vars["think"])
}

func TestUnit_BuildTemplateVars_DefaultFallbacksAreAlwaysPresent(t *testing.T) {
	vars := buildTemplateVars(chatOpts{
		EffectiveDefaultModel:    "qwen2.5:7b",
		EffectiveDefaultProvider: "ollama",
	})

	require.Equal(t, "qwen2.5:7b", vars["model"])
	require.Equal(t, "qwen2.5:7b", vars["default_model"])
	require.Equal(t, "ollama", vars["provider"])
	require.Equal(t, "ollama", vars["default_provider"])
}

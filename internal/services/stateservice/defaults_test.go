package stateservice

import "testing"

func TestRuntimeDefaultsWithCLIConfigUsesPresentEmptyValues(t *testing.T) {
	fallback := RuntimeDefaults{
		ChainRef:    "startup-chain",
		Model:       "startup-model",
		Provider:    "startup-provider",
		AltModel:    "startup-alt-model",
		AltProvider: "startup-alt-provider",
		MaxTokens:   "4096",
		Think:       "medium",
	}
	got := fallback.WithCLIConfig(CLIConfigSnapshot{
		Present: map[string]bool{
			"default-model":        true,
			"default-provider":     true,
			"default-alt-model":    true,
			"default-alt-provider": true,
			"default-max-tokens":   true,
			"default-think":        true,
			"default-chain":        true,
		},
	}).Trimmed()

	if got != (RuntimeDefaults{}) {
		t.Fatalf("expected explicit empty CLI values to clear fallback defaults, got %#v", got)
	}
}

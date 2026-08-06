package setupcheck

import (
	"strings"
	"testing"
)

// canonicalOllamaSuggestModel and bannedStaleSuggestions duplicate the
// canonical list in internal/surfaces/contenoxcli/canonical_suggestions_test.go
// (the source of truth) — keep the two files in sync.
const canonicalOllamaSuggestModel = "qwen3:8b"

var bannedStaleSuggestions = []string{
	"qwen2.5:7b",
	"gpt-4o-mini",
	"gpt-4.1-mini",
	"gemini-3.1-pro-preview",
	"gemini-2.5-flash",
	"backend add local",
}

// TestUnit_CanonicalSuggestions_DefaultOllamaModel pins the package's suggested
// Ollama model to the canonical value.
func TestUnit_CanonicalSuggestions_DefaultOllamaModel(t *testing.T) {
	if DefaultOllamaSuggestModel != canonicalOllamaSuggestModel {
		t.Errorf("DefaultOllamaSuggestModel = %q, want canonical %q", DefaultOllamaSuggestModel, canonicalOllamaSuggestModel)
	}
}

// TestUnit_CanonicalSuggestions_NoStaleStrings runs every suggestion-producing
// function across all provider types and fails on any banned stale string.
func TestUnit_CanonicalSuggestions_NoStaleStrings(t *testing.T) {
	providers := []string{"", "ollama", "openai", "anthropic", "gemini", "vertex-google", "bedrock", "vllm", "local"}

	outputs := map[string]string{}
	for _, p := range providers {
		outputs["providerAddCommand("+p+")"] = providerAddCommand(p)
		outputs["noChatModelsCommand("+p+")"] = noChatModelsCommand(p)
		outputs["primaryDiagnosticCommand("+p+")"] = primaryDiagnosticCommand(p)
		outputs["embedModelCommand("+p+")"] = embedModelCommand(p, "some-embed-model")
		outputs["repairBackendCommand("+p+")"] = repairBackendCommand(&BackendCheck{Name: p, Type: p})
	}
	outputs["repairBackendCommand(ollama-cloud)"] = repairBackendCommand(&BackendCheck{Name: "ollama-cloud", Type: "ollama", BaseURL: "https://ollama.com/api"})

	for name, text := range outputs {
		for _, banned := range bannedStaleSuggestions {
			if strings.Contains(text, banned) {
				t.Errorf("%s contains banned stale suggestion %q", name, banned)
			}
		}
	}

	if !strings.Contains(providerAddCommand("ollama"), "backend add ollama") {
		t.Errorf("providerAddCommand(ollama) = %q, want it to suggest \"backend add ollama\"", providerAddCommand("ollama"))
	}
}

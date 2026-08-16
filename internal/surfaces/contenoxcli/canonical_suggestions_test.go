package contenoxcli

import (
	"fmt"
	"strings"
	"testing"

	"github.com/contenox/contenox/internal/services/setupcheck"
)

// canonicalSuggestedModels is the single source of truth for the model names
// the CLI suggests in help text, wizard defaults, and docs examples.
var canonicalSuggestedModels = map[string]string{
	"ollama":        "qwen3:8b",
	"ollama-cloud":  "gpt-oss:20b",
	"openai":        "gpt-5-mini",
	"gemini":        "gemini-flash-latest",
	"vertex-google": "gemini-3.6-flash",
	"anthropic":     "claude-sonnet-4-5",
	"bedrock":       "us.anthropic.claude-3-5-sonnet-20241022-v2:0",
	"autocomplete":  "qwen2.5-coder:7b",
}

// canonicalFirstBackendAdd is the suggested first `backend add` invocation
// (backend NAME "ollama", never the stale "local").
const canonicalFirstBackendAdd = "backend add ollama"

// bannedStaleSuggestions are superseded model/name suggestions that must never
// reappear in any user-facing suggestion surface.
var bannedStaleSuggestions = []string{
	"qwen2.5:7b",
	"gpt-4o-mini",
	"gpt-4.1-mini",
	"gemini-3.1-pro-preview",
	"gemini-2.5-flash",
	"backend add local",
}

// suggestionSurfaces gathers every help/wizard text that carries model or
// backend-name suggestions. Extend it when a new suggestion surface is added.
func suggestionSurfaces() map[string]string {
	surfaces := map[string]string{
		"rootCmd.Long":       rootCmd.Long,
		"initCmd.Long":       initCmd.Long,
		"setupCmd.Long":      setupCmd.Long,
		"doctorCmd.Long":     doctorCmd.Long,
		"configCmd.Long":     configCmd.Long,
		"configSetCmd.Long":  configSetCmd.Long,
		"modelCmd.Long":      modelCmd.Long,
		"modelListCmd.Long":  modelListCmd.Long,
		"backendCmd.Long":    backendCmd.Long,
		"backendAddCmd.Long": backendAddCmd.Long,
	}
	for i, sp := range setupProviders {
		surfaces[fmt.Sprintf("setupProviders[%d] (%s) defaultModel", i, sp.label)] = sp.defaultModel
	}
	for name, pc := range providerConfigs {
		surfaces[fmt.Sprintf("providerConfigs[%s].defaultModel", name)] = pc.defaultModel
	}
	return surfaces
}

// TestUnit_CanonicalSuggestions_NoStaleStrings fails when a banned stale
// suggestion sneaks back into any help/wizard suggestion surface.
func TestUnit_CanonicalSuggestions_NoStaleStrings(t *testing.T) {
	for name, text := range suggestionSurfaces() {
		for _, banned := range bannedStaleSuggestions {
			if strings.Contains(text, banned) {
				t.Errorf("%s contains banned stale suggestion %q", name, banned)
			}
		}
	}
}

// TestUnit_CanonicalSuggestions_InitProviderDefaults pins init's per-provider
// default models to the canonical map wherever a canonical row exists.
func TestUnit_CanonicalSuggestions_InitProviderDefaults(t *testing.T) {
	for name, pc := range providerConfigs {
		want, ok := canonicalSuggestedModels[name]
		if !ok || pc.defaultModel == "" {
			continue
		}
		if pc.defaultModel != want {
			t.Errorf("providerConfigs[%s].defaultModel = %q, canonical is %q", name, pc.defaultModel, want)
		}
	}
}

// TestUnit_CanonicalSuggestions_WizardDefaults pins every wizard entry with a
// fixed default model to the canonical map.
func TestUnit_CanonicalSuggestions_WizardDefaults(t *testing.T) {
	for i, sp := range setupProviders {
		canonicalKey := sp.key
		if sp.key == "ollama" && sp.fixedBaseURL != "" {
			canonicalKey = "ollama-cloud"
		}
		want, ok := canonicalSuggestedModels[canonicalKey]
		if !ok {
			// vllm: no fixed default — the user enters the served model id.
			if sp.defaultModel != "" {
				t.Errorf("setupProviders[%d] (%s) has defaultModel %q but no canonical entry — add it to canonicalSuggestedModels", i, sp.label, sp.defaultModel)
			}
			continue
		}
		if sp.defaultModel != want {
			t.Errorf("setupProviders[%d] (%s) defaultModel = %q, want canonical %q", i, sp.label, sp.defaultModel, want)
		}
	}
}

// TestUnit_CanonicalSuggestions_SetupcheckOllamaModel pins setupcheck's
// suggested Ollama model to the canonical one.
func TestUnit_CanonicalSuggestions_SetupcheckOllamaModel(t *testing.T) {
	if setupcheck.DefaultOllamaSuggestModel != canonicalSuggestedModels["ollama"] {
		t.Errorf("setupcheck.DefaultOllamaSuggestModel = %q, want canonical %q", setupcheck.DefaultOllamaSuggestModel, canonicalSuggestedModels["ollama"])
	}
}

// TestUnit_CanonicalSuggestions_PositiveMentions asserts the canonical
// first-backend command and autocomplete example are what the help suggests.
func TestUnit_CanonicalSuggestions_PositiveMentions(t *testing.T) {
	for _, name := range []string{"rootCmd.Long", "initCmd.Long"} {
		text := suggestionSurfaces()[name]
		if !strings.Contains(text, canonicalFirstBackendAdd) {
			t.Errorf("%s does not suggest %q", name, canonicalFirstBackendAdd)
		}
		if !strings.Contains(text, canonicalSuggestedModels["autocomplete"]) {
			t.Errorf("%s does not suggest autocomplete example %q", name, canonicalSuggestedModels["autocomplete"])
		}
	}
	if !strings.Contains(configSetCmd.Long, canonicalSuggestedModels["autocomplete"]) {
		t.Errorf("configSetCmd.Long does not suggest autocomplete example %q", canonicalSuggestedModels["autocomplete"])
	}
}

package acpsvc

import "testing"

// The seeded chains resolve execute_config.model via
// {{var:alt_model|var:default_model}}, so default_model must be present
// whenever any model is known, or chain execution fails despite a
// configured default.
func TestUnit_ChainTemplateVars_SeedsDefaultModelAndProvider(t *testing.T) {
	tr := &Transport{defaultModel: "gemma4-e4b", defaultProvider: "llama"}
	sess := &sessionEntry{}

	vars := tr.chainTemplateVars(sess)

	if vars["model"] != "gemma4-e4b" || vars["provider"] != "llama" {
		t.Fatalf("model/provider = %q/%q, want gemma4-e4b/llama", vars["model"], vars["provider"])
	}
	if vars["default_model"] != "gemma4-e4b" {
		t.Fatalf("default_model = %q, want gemma4-e4b", vars["default_model"])
	}
	if vars["default_provider"] != "llama" {
		t.Fatalf("default_provider = %q, want llama", vars["default_provider"])
	}
	if _, ok := vars["alt_model"]; ok {
		t.Fatalf("alt_model should be absent when not configured, got %q", vars["alt_model"])
	}
}

func TestUnit_ChainTemplateVars_FallsBackToSessionSelection(t *testing.T) {
	tr := &Transport{}
	sess := &sessionEntry{Provider: "openai", Model: "gpt-5-mini"}

	vars := tr.chainTemplateVars(sess)

	if vars["default_model"] != "gpt-5-mini" {
		t.Fatalf("default_model = %q, want session model gpt-5-mini", vars["default_model"])
	}
	if vars["default_provider"] != "openai" {
		t.Fatalf("default_provider = %q, want session provider openai", vars["default_provider"])
	}
}

// A session model selection (e.g. Zed's model dropdown) is session-only and does
// not touch the transport-configured default. default_model/default_provider —
// the recovery fallback for {{var:alt_model|var:default_model}} — must follow the
// session-effective selection, not the (possibly stale/unreachable) configured
// default, or recovery/summarise_failure resolves a provider with no models in
// runtime state while the main tasks succeed on the session model. Power users who
// want a distinct recovery model still get it via alt_model, which wins the macro.
func TestUnit_ChainTemplateVars_SessionSelectionSeedsRecoveryFallback(t *testing.T) {
	tr := &Transport{defaultModel: "gemma4-e4b", defaultProvider: "llama", defaultAltModel: "claude-sonnet-5", defaultAltProvider: "anthropic"}
	sess := &sessionEntry{Provider: "openai", Model: "gpt-5-mini"}

	vars := tr.chainTemplateVars(sess)

	if vars["model"] != "gpt-5-mini" {
		t.Fatalf("model = %q, want session selection gpt-5-mini", vars["model"])
	}
	if vars["default_model"] != "gpt-5-mini" {
		t.Fatalf("default_model = %q, want session selection gpt-5-mini", vars["default_model"])
	}
	if vars["default_provider"] != "openai" {
		t.Fatalf("default_provider = %q, want session selection openai", vars["default_provider"])
	}
	if vars["alt_model"] != "claude-sonnet-5" || vars["alt_provider"] != "anthropic" {
		t.Fatalf("alt_model/alt_provider = %q/%q, want claude-sonnet-5/anthropic", vars["alt_model"], vars["alt_provider"])
	}
}

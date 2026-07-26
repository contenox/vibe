package stateservice

import (
	"context"
	"strings"
)

// RuntimeDefaults are the model/chain template defaults used when a request
// does not pass an explicit override.
type RuntimeDefaults struct {
	ChainRef             string
	FIMChainRef          string
	Model                string
	Provider             string
	AltModel             string
	AltProvider          string
	AutocompleteModel    string
	AutocompleteProvider string
	MaxTokens            string
	Think                string
}

// ResolveRuntimeDefaults overlays the current CLI config onto startup
// fallbacks. If state reads fail, callers keep the known-good startup values.
func ResolveRuntimeDefaults(ctx context.Context, service Service, fallback RuntimeDefaults) RuntimeDefaults {
	if service == nil {
		return fallback.Trimmed()
	}
	snap, err := service.CLIConfig(ctx)
	if err != nil {
		return fallback.Trimmed()
	}
	return fallback.WithCLIConfig(snap).Trimmed()
}

func (d RuntimeDefaults) WithCLIConfig(snap CLIConfigSnapshot) RuntimeDefaults {
	if snap.has("default-chain", snap.DefaultChain) {
		d.ChainRef = snap.DefaultChain
	}
	if snap.has("default-model", snap.DefaultModel) {
		d.Model = snap.DefaultModel
	}
	if snap.has("default-provider", snap.DefaultProvider) {
		d.Provider = snap.DefaultProvider
	}
	if snap.has("default-alt-model", snap.DefaultAltModel) {
		d.AltModel = snap.DefaultAltModel
	}
	if snap.has("default-alt-provider", snap.DefaultAltProvider) {
		d.AltProvider = snap.DefaultAltProvider
	}
	if snap.has("default-autocomplete-model", snap.DefaultAutocompleteModel) {
		d.AutocompleteModel = snap.DefaultAutocompleteModel
	}
	if snap.has("default-autocomplete-provider", snap.DefaultAutocompleteProvider) {
		d.AutocompleteProvider = snap.DefaultAutocompleteProvider
	}
	if snap.has("default-max-tokens", snap.DefaultMaxTokens) {
		d.MaxTokens = snap.DefaultMaxTokens
	}
	if snap.has("default-think", snap.DefaultThink) {
		d.Think = snap.DefaultThink
	}
	return d
}

func (d RuntimeDefaults) Trimmed() RuntimeDefaults {
	return RuntimeDefaults{
		ChainRef:             strings.TrimSpace(d.ChainRef),
		FIMChainRef:          strings.TrimSpace(d.FIMChainRef),
		Model:                strings.TrimSpace(d.Model),
		Provider:             strings.TrimSpace(d.Provider),
		AltModel:             strings.TrimSpace(d.AltModel),
		AltProvider:          strings.TrimSpace(d.AltProvider),
		AutocompleteModel:    strings.TrimSpace(d.AutocompleteModel),
		AutocompleteProvider: strings.TrimSpace(d.AutocompleteProvider),
		MaxTokens:            strings.TrimSpace(d.MaxTokens),
		Think:                strings.TrimSpace(d.Think),
	}
}

func (d RuntimeDefaults) TemplateVars() map[string]string {
	d = d.Trimmed()
	vars := map[string]string{}
	// The seeded chains reference {{var:alt_model|var:default_model}} (and the
	// provider equivalent), so default_model/default_provider must be set
	// whenever a model is known, matching the CLI chat and ACP paths.
	if d.Model != "" {
		vars["model"] = d.Model
		vars["default_model"] = d.Model
	}
	if d.Provider != "" {
		vars["provider"] = d.Provider
		vars["default_provider"] = d.Provider
	}
	if d.AltModel != "" {
		vars["alt_model"] = d.AltModel
	}
	if d.AltProvider != "" {
		vars["alt_provider"] = d.AltProvider
	}
	if d.MaxTokens != "" {
		vars["max_tokens"] = d.MaxTokens
	}
	if d.Think != "" {
		vars["think"] = d.Think
	}
	return vars
}

func (s CLIConfigSnapshot) has(key, value string) bool {
	if s.Present != nil {
		return s.Present[key]
	}
	return strings.TrimSpace(value) != ""
}

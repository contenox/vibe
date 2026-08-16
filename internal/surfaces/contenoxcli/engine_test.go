package contenoxcli

import (
	"testing"
)

func TestReadinessDefaults(t *testing.T) {
	cases := []struct {
		name         string
		opts         chatOpts
		wantModel    string
		wantProvider string
	}{
		{
			name: "explicit --model on fresh DB is credited",
			opts: chatOpts{
				EffectiveDefaultModel:    "phi-4-mini",
				EffectiveConfiguredModel: "",
			},
			wantModel: "phi-4-mini",
		},
		{
			name: "hardcoded fallback model on fresh DB is not credited",
			opts: chatOpts{
				EffectiveDefaultModel:    defaultModel,
				EffectiveConfiguredModel: "",
			},
			wantModel: "",
		},
		{
			name: "model from persisted config needs no override",
			opts: chatOpts{
				EffectiveDefaultModel:    "persisted",
				EffectiveConfiguredModel: "persisted",
			},
			wantModel: "",
		},
		{
			name: "explicit --provider on fresh DB is credited",
			opts: chatOpts{
				EffectiveDefaultProvider:    "ollama",
				EffectiveConfiguredProvider: "",
			},
			wantProvider: "ollama",
		},
		{
			name: "provider from persisted config needs no override",
			opts: chatOpts{
				EffectiveDefaultProvider:    "ollama",
				EffectiveConfiguredProvider: "ollama",
			},
			wantProvider: "",
		},
		{
			name: "model and provider flags both credited together",
			opts: chatOpts{
				EffectiveDefaultModel:    "phi-4-mini",
				EffectiveConfiguredModel: "",
				EffectiveDefaultProvider: "vllm",
			},
			wantModel:    "phi-4-mini",
			wantProvider: "vllm",
		},
		{
			// The reported bug: a healthy explicit override must beat a broken
			// persisted default, not be ignored because config is non-empty.
			name: "explicit flags override a broken persisted config",
			opts: chatOpts{
				EffectiveDefaultModel:       "gemini-2.5-flash",
				EffectiveConfiguredModel:    "unservable-model",
				EffectiveDefaultProvider:    "vertex-google",
				EffectiveConfiguredProvider: "vllm",
			},
			wantModel:    "gemini-2.5-flash",
			wantProvider: "vertex-google",
		},
		{
			// Override only the provider; the model stays on persisted config and
			// needs no readiness credit (effective == configured).
			name: "provider override alone, model unchanged from config",
			opts: chatOpts{
				EffectiveDefaultModel:       "persisted",
				EffectiveConfiguredModel:    "persisted",
				EffectiveDefaultProvider:    "vertex-google",
				EffectiveConfiguredProvider: "vllm",
			},
			wantModel:    "",
			wantProvider: "vertex-google",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			model, provider := readinessDefaults(tc.opts)
			if model != tc.wantModel {
				t.Errorf("model = %q, want %q", model, tc.wantModel)
			}
			if provider != tc.wantProvider {
				t.Errorf("provider = %q, want %q", provider, tc.wantProvider)
			}
		})
	}
}

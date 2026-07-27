package contenoxcli

import (
	"context"
	"testing"

	"github.com/contenox/beam/internal/libtracker"
	"github.com/contenox/beam/internal/services/gointel"
	"github.com/stretchr/testify/require"
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

// TestUnit_LocalToolset_GitIsAlwaysOnAndShellIsGated pins the two registration
// decisions this file makes.
//
// git is unconditional: the whole point of the toolset is that an agent can SEE
// the repository, and gating that behind --shell is what produced "I can't run
// git" on a surface that had git sitting right there. What the agent may DO with
// it is the envelope's call, not this one — the seeded policies allow the reads
// and hold the four mutations at an approval.
//
// local_shell stays gated: enabling raw command execution is still an explicit
// choice (beam makes it, `contenox chat` does not).
func TestUnit_LocalToolset_GitIsAlwaysOnAndShellIsGated(t *testing.T) {
	tracker := libtracker.NoopTracker{}

	goIndex := gointel.NewIndex(gointel.Config{})
	t.Cleanup(goIndex.Shutdown)

	off := localToolset(chatOpts{EffectiveEnableLocalExec: false}, nil, tracker, goIndex)
	require.Contains(t, off, "git", "git must be registered even with the shell off")
	require.Contains(t, off, "local_fs")
	require.Contains(t, off, gointel.ToolsProviderName, "gointel is a read surface, always on")
	require.NotContains(t, off, "local_shell", "the shell stays opt-in")

	on := localToolset(chatOpts{EffectiveEnableLocalExec: true, EffectiveHITL: true}, nil, tracker, goIndex)
	require.Contains(t, on, "git")
	require.Contains(t, on, "local_shell")

	// The registered provider is the real toolset, addressed by the same name
	// the seeded envelopes' rules use.
	supported, err := off["git"].Supports(context.Background())
	require.NoError(t, err)
	require.Contains(t, supported, "git_status")
	require.Contains(t, supported, "git_commit")
}

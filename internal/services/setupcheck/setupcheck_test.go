package setupcheck

import (
	"strings"
	"testing"

	"github.com/contenox/contenox/internal/models/runtimestate"
	"github.com/contenox/contenox/internal/store/runtimetypes"
)

func TestSystem_Evaluate_missingDefaults(t *testing.T) {
	r := Evaluate(Input{States: []runtimestate.BackendRuntimeState{{}}})
	if len(r.Issues) < 2 {
		t.Fatalf("expected at least 2 issues, got %v", r.Issues)
	}
}

func TestSystem_Evaluate_noBackends(t *testing.T) {
	r := Evaluate(Input{
		DefaultModel:    "m",
		DefaultProvider: "ollama",
		States:          nil,
	})
	var found bool
	for _, i := range r.Issues {
		if i.Code == "no_backends" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected no_backends issue, got %#v", r.Issues)
	}
}

func TestUnit_Evaluate_ResolvesDefaultMaxOutputTokens(t *testing.T) {
	states := []runtimestate.BackendRuntimeState{{
		Backend: runtimetypes.Backend{Type: "openai"},
		PulledModels: []runtimestate.ModelPullStatus{{
			Model:           "gpt-5",
			CanChat:         true,
			MaxOutputTokens: 128000,
		}},
	}}

	r := Evaluate(Input{
		DefaultModel:    "gpt-5",
		DefaultProvider: "openai",
		States:          states,
	})

	if r.DefaultMaxOutputTokens != 128000 {
		t.Fatalf("DefaultMaxOutputTokens = %d, want 128000", r.DefaultMaxOutputTokens)
	}
	if got := ResolveMaxOutputTokens(states, "vertex", "gpt-5"); got != 0 {
		t.Fatalf("vertex should not match openai state, got %d", got)
	}
}

func TestSystem_Evaluate_allUnreachable(t *testing.T) {
	r := Evaluate(Input{
		DefaultModel:    "m",
		DefaultProvider: "ollama",
		States: []runtimestate.BackendRuntimeState{
			{Error: "down"},
			{Error: "timeout"},
		},
	})
	var found bool
	for _, i := range r.Issues {
		if i.Code == "all_backends_unreachable" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected all_backends_unreachable, got %#v", r.Issues)
	}
}

func TestSystem_Evaluate_doctorSkipsUnreachableWhenNoState(t *testing.T) {
	n := 2
	r := Evaluate(Input{
		DefaultModel:           "m",
		DefaultProvider:        "ollama",
		States:                 nil,
		RegisteredBackendCount: &n,
	})
	for _, i := range r.Issues {
		if i.Code == "all_backends_unreachable" {
			t.Fatalf("unexpected all_backends_unreachable when states empty")
		}
	}
	var foundEmpty bool
	for _, i := range r.Issues {
		if i.Code == "runtime_state_empty" {
			foundEmpty = true
		}
	}
	if !foundEmpty {
		t.Fatalf("expected runtime_state_empty when DB reports backends but state snapshot empty, got %#v", r.Issues)
	}
	if r.BackendCount != 2 {
		t.Fatalf("backend count: got %d", r.BackendCount)
	}
}

func TestSystem_Evaluate_noBackendsSkippedWhenRegisteredInDB(t *testing.T) {
	n := 1
	r := Evaluate(Input{
		DefaultModel:           "m",
		DefaultProvider:        "ollama",
		States:                 nil,
		RegisteredBackendCount: &n,
	})
	for _, i := range r.Issues {
		if i.Code == "no_backends" {
			t.Fatalf("unexpected no_backends when DB reports backends")
		}
	}
	var foundEmpty bool
	for _, i := range r.Issues {
		if i.Code == "runtime_state_empty" {
			foundEmpty = true
		}
	}
	if !foundEmpty {
		t.Fatalf("expected runtime_state_empty, got %#v", r.Issues)
	}
}

func TestSystem_Evaluate_noBackends_openaiHint(t *testing.T) {
	r := Evaluate(Input{
		DefaultModel:    "gpt-4o",
		DefaultProvider: "openai",
		States:          nil,
	})
	var nb *Issue
	for i := range r.Issues {
		if r.Issues[i].Code == "no_backends" {
			nb = &r.Issues[i]
			break
		}
	}
	if nb == nil || nb.CLICommand == "" || !strings.Contains(nb.CLICommand, "openai") {
		t.Fatalf("expected openai backend hint, got %#v", nb)
	}
}

func TestSystem_Evaluate_noChatModels_ollama(t *testing.T) {
	r := Evaluate(Input{
		DefaultModel:    "llama3",
		DefaultProvider: "ollama",
		States: []runtimestate.BackendRuntimeState{
			{
				Backend: runtimetypes.Backend{Type: "ollama"},
				PulledModels: []runtimestate.ModelPullStatus{
					{Name: "embed", CanChat: false, CanEmbed: true},
				},
			},
		},
	})
	var found bool
	for _, i := range r.Issues {
		if i.Code == "no_chat_models" {
			found = true
			if i.CLICommand == "" || !strings.Contains(i.CLICommand, "ollama") {
				t.Fatalf("expected ollama hint, got %#v", i)
			}
		}
	}
	if !found {
		t.Fatalf("expected no_chat_models, got %#v", r.Issues)
	}
}

func TestSystem_Evaluate_noChatModels_openaiHint(t *testing.T) {
	r := Evaluate(Input{
		DefaultModel:    "gpt-4o",
		DefaultProvider: "openai",
		States: []runtimestate.BackendRuntimeState{
			{Backend: runtimetypes.Backend{Type: "openai"}, PulledModels: nil},
		},
	})
	var ncm *Issue
	for i := range r.Issues {
		if r.Issues[i].Code == "no_chat_models" {
			ncm = &r.Issues[i]
			break
		}
	}
	if ncm == nil || !strings.Contains(ncm.CLICommand, "contenox model list") {
		t.Fatalf("expected openai no_chat_models diagnostic command, got %#v", ncm)
	}
}

func TestSystem_Evaluate_noChatModels_skippedWhenChatModelExists(t *testing.T) {
	r := Evaluate(Input{
		DefaultModel:    "m",
		DefaultProvider: "ollama",
		States: []runtimestate.BackendRuntimeState{
			{
				Backend: runtimetypes.Backend{Type: "ollama"},
				PulledModels: []runtimestate.ModelPullStatus{
					{Name: "llama3", CanChat: true},
				},
			},
		},
	})
	for _, i := range r.Issues {
		if i.Code == "no_chat_models" {
			t.Fatalf("unexpected no_chat_models when chat model exists: %#v", r.Issues)
		}
	}
}

func TestSystem_Evaluate_noChatModels_skippedWhenAllUnreachable(t *testing.T) {
	r := Evaluate(Input{
		DefaultModel:    "m",
		DefaultProvider: "ollama",
		States: []runtimestate.BackendRuntimeState{
			{Backend: runtimetypes.Backend{Type: "ollama"}, Error: "down"},
		},
	})
	for _, i := range r.Issues {
		if i.Code == "no_chat_models" {
			t.Fatalf("unexpected no_chat_models when all unreachable")
		}
	}
}

func TestSystem_Evaluate_noChatModels_skippedWhenStatesEmpty(t *testing.T) {
	n := 1
	r := Evaluate(Input{
		DefaultModel:           "m",
		DefaultProvider:        "ollama",
		States:                 nil,
		RegisteredBackendCount: &n,
	})
	for _, i := range r.Issues {
		if i.Code == "no_chat_models" {
			t.Fatalf("unexpected no_chat_models with empty states")
		}
	}
	var foundEmpty bool
	for _, i := range r.Issues {
		if i.Code == "runtime_state_empty" {
			foundEmpty = true
		}
	}
	if !foundEmpty {
		t.Fatalf("expected runtime_state_empty, got %#v", r.Issues)
	}
}

func TestSystem_Evaluate_defaultProviderBackendMissing(t *testing.T) {
	r := Evaluate(Input{
		DefaultModel:    "gpt-5",
		DefaultProvider: "openai",
		RegisteredBackends: []runtimetypes.Backend{
			{Name: "local", Type: "ollama", BaseURL: "http://127.0.0.1:11434"},
		},
		RegisteredBackendCount: func() *int { n := 1; return &n }(),
	})

	var found bool
	for _, issue := range r.Issues {
		if issue.Code == "default_provider_backend_missing" {
			found = true
			if issue.Category != CategoryRegistration {
				t.Fatalf("expected registration category, got %#v", issue)
			}
		}
	}
	if !found {
		t.Fatalf("expected default_provider_backend_missing, got %#v", r.Issues)
	}
}

func TestSystem_Evaluate_defaultProviderAPIKeyMissing(t *testing.T) {
	backend := runtimetypes.Backend{ID: "b-openai", Name: "openai", Type: "openai", BaseURL: "https://api.openai.com/v1"}
	r := Evaluate(Input{
		DefaultModel:       "gpt-5",
		DefaultProvider:    "openai",
		RegisteredBackends: []runtimetypes.Backend{backend},
		States: []runtimestate.BackendRuntimeState{
			{Backend: backend, Error: "API key not configured"},
		},
	})

	var found *Issue
	for i := range r.Issues {
		if r.Issues[i].Code == "default_provider_api_key_missing" {
			found = &r.Issues[i]
			break
		}
	}
	if found == nil {
		t.Fatalf("expected default_provider_api_key_missing, got %#v", r.Issues)
	}
	if found.CLICommand == "" || !strings.Contains(found.CLICommand, "OPENAI_API_KEY") {
		t.Fatalf("expected OPENAI_API_KEY repair hint, got %#v", found)
	}
}

func TestSystem_Evaluate_defaultProviderAPIKeyMissing_hostedOllama(t *testing.T) {
	backend := runtimetypes.Backend{
		ID:      "b-ollama-cloud",
		Name:    "ollama-cloud",
		Type:    "ollama",
		BaseURL: "https://ollama.com/api",
	}
	r := Evaluate(Input{
		DefaultModel:       "qwen3",
		DefaultProvider:    "ollama",
		RegisteredBackends: []runtimetypes.Backend{backend},
		States: []runtimestate.BackendRuntimeState{
			{Backend: backend, Error: "API key not configured"},
		},
	})

	var found *Issue
	for i := range r.Issues {
		if r.Issues[i].Code == "default_provider_api_key_missing" {
			found = &r.Issues[i]
			break
		}
	}
	if found == nil {
		t.Fatalf("expected default_provider_api_key_missing, got %#v", r.Issues)
	}
	if found.FixPath != "/backends?tab=cloud-providers" {
		t.Fatalf("expected cloud providers fix path, got %#v", found)
	}
	if found.CLICommand == "" || !strings.Contains(found.CLICommand, "OLLAMA_API_KEY") {
		t.Fatalf("expected OLLAMA_API_KEY repair hint, got %#v", found)
	}
	if len(r.BackendChecks) != 1 || !strings.Contains(r.BackendChecks[0].Hint, "Cloud providers") {
		t.Fatalf("expected backend hint to reference Cloud providers, got %#v", r.BackendChecks)
	}
}

func TestSystem_Evaluate_defaultModelNotAvailable(t *testing.T) {
	backend := runtimetypes.Backend{ID: "b-openai", Name: "openai", Type: "openai", BaseURL: "https://api.openai.com/v1"}
	r := Evaluate(Input{
		DefaultModel:       "gpt-5",
		DefaultProvider:    "openai",
		RegisteredBackends: []runtimetypes.Backend{backend},
		States: []runtimestate.BackendRuntimeState{
			{
				Backend: backend,
				PulledModels: []runtimestate.ModelPullStatus{
					{Model: "gpt-4o", CanChat: true},
				},
			},
		},
	})

	var found *Issue
	for i := range r.Issues {
		if r.Issues[i].Code == "default_model_not_available" {
			found = &r.Issues[i]
			break
		}
	}
	if found == nil {
		t.Fatalf("expected default_model_not_available, got %#v", r.Issues)
	}
	if !strings.Contains(found.Message, "gpt-4o") {
		t.Fatalf("expected available model in message, got %#v", found)
	}
}

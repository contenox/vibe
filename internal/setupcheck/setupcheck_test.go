package setupcheck

import (
	"strings"
	"testing"

	"github.com/contenox/contenox/runtimetypes"
	"github.com/contenox/contenox/statetype"
)

func TestEvaluate_missingDefaults(t *testing.T) {
	r := Evaluate(Input{States: []statetype.BackendRuntimeState{{}}})
	if len(r.Issues) < 2 {
		t.Fatalf("expected at least 2 issues, got %v", r.Issues)
	}
}

func TestEvaluate_noBackends(t *testing.T) {
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

func TestEvaluate_allUnreachable(t *testing.T) {
	r := Evaluate(Input{
		DefaultModel:    "m",
		DefaultProvider: "ollama",
		States: []statetype.BackendRuntimeState{
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

func TestEvaluate_doctorSkipsUnreachableWhenNoState(t *testing.T) {
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

func TestEvaluate_noBackendsSkippedWhenRegisteredInDB(t *testing.T) {
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

func TestEvaluate_noBackends_openaiHint(t *testing.T) {
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

func TestEvaluate_noChatModels_ollama(t *testing.T) {
	r := Evaluate(Input{
		DefaultModel:    "llama3",
		DefaultProvider: "ollama",
		States: []statetype.BackendRuntimeState{
			{
				Backend: runtimetypes.Backend{Type: "ollama"},
				PulledModels: []statetype.ModelPullStatus{
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

func TestEvaluate_noChatModels_openaiHint(t *testing.T) {
	r := Evaluate(Input{
		DefaultModel:    "gpt-4o",
		DefaultProvider: "openai",
		States: []statetype.BackendRuntimeState{
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

func TestEvaluate_noChatModels_skippedWhenChatModelExists(t *testing.T) {
	r := Evaluate(Input{
		DefaultModel:    "m",
		DefaultProvider: "ollama",
		States: []statetype.BackendRuntimeState{
			{
				Backend: runtimetypes.Backend{Type: "ollama"},
				PulledModels: []statetype.ModelPullStatus{
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

func TestEvaluate_noChatModels_skippedWhenAllUnreachable(t *testing.T) {
	r := Evaluate(Input{
		DefaultModel:    "m",
		DefaultProvider: "ollama",
		States: []statetype.BackendRuntimeState{
			{Backend: runtimetypes.Backend{Type: "ollama"}, Error: "down"},
		},
	})
	for _, i := range r.Issues {
		if i.Code == "no_chat_models" {
			t.Fatalf("unexpected no_chat_models when all unreachable")
		}
	}
}

func TestEvaluate_noChatModels_skippedWhenStatesEmpty(t *testing.T) {
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

func TestEvaluate_defaultProviderBackendMissing(t *testing.T) {
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

func TestEvaluate_defaultProviderAPIKeyMissing(t *testing.T) {
	backend := runtimetypes.Backend{ID: "b-openai", Name: "openai", Type: "openai", BaseURL: "https://api.openai.com/v1"}
	r := Evaluate(Input{
		DefaultModel:       "gpt-5",
		DefaultProvider:    "openai",
		RegisteredBackends: []runtimetypes.Backend{backend},
		States: []statetype.BackendRuntimeState{
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

func TestEvaluate_defaultProviderAPIKeyMissing_hostedOllama(t *testing.T) {
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
		States: []statetype.BackendRuntimeState{
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

func TestEvaluate_defaultModelNotAvailable(t *testing.T) {
	backend := runtimetypes.Backend{ID: "b-openai", Name: "openai", Type: "openai", BaseURL: "https://api.openai.com/v1"}
	r := Evaluate(Input{
		DefaultModel:       "gpt-5",
		DefaultProvider:    "openai",
		RegisteredBackends: []runtimetypes.Backend{backend},
		States: []statetype.BackendRuntimeState{
			{
				Backend: backend,
				PulledModels: []statetype.ModelPullStatus{
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

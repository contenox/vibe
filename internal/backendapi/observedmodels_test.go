package backendapi

import (
	"testing"

	"github.com/contenox/contenox/runtimetypes"
	"github.com/contenox/contenox/statetype"
)

func TestObservedModelNamesIgnoresDeclaredModelsOnBackendError(t *testing.T) {
	t.Parallel()

	state := statetype.BackendRuntimeState{
		Models: []string{"gpt-5", "qwen2.5:7b"},
		Error:  `Get "http://127.0.0.1:11434/api/tags": connect: connection refused`,
	}

	got := observedModelNames(state)
	if len(got) != 0 {
		t.Fatalf("observedModelNames() = %v, want empty slice", got)
	}
}

func TestSanitizeRuntimeStatesReplacesModelsWithObservedInventory(t *testing.T) {
	t.Parallel()

	states := []statetype.BackendRuntimeState{
		{
			Models: []string{"declared-only"},
			PulledModels: []statetype.ModelPullStatus{
				{Model: "llama3.2:3b"},
			},
		},
	}

	got := sanitizeRuntimeStates(states)
	if len(got) != 1 {
		t.Fatalf("sanitizeRuntimeStates() len = %d, want 1", len(got))
	}
	if len(got[0].Models) != 1 || got[0].Models[0] != "llama3.2:3b" {
		t.Fatalf("sanitizeRuntimeStates()[0].Models = %v, want [llama3.2:3b]", got[0].Models)
	}
}

func TestSanitizeRuntimeStatesDropsStringOnlyModelFallback(t *testing.T) {
	t.Parallel()

	states := []statetype.BackendRuntimeState{
		{
			Models: []string{"string-only-model"},
		},
	}

	got := sanitizeRuntimeStates(states)
	if len(got) != 1 {
		t.Fatalf("sanitizeRuntimeStates() len = %d, want 1", len(got))
	}
	if len(got[0].Models) != 0 {
		t.Fatalf("sanitizeRuntimeStates()[0].Models = %v, want empty slice", got[0].Models)
	}
}

func TestListObservedModelsMergesObservedEntriesAcrossBackends(t *testing.T) {
	t.Parallel()

	states := []statetype.BackendRuntimeState{
		{
			Backend: runtimetypes.Backend{Name: "local"},
			PulledModels: []statetype.ModelPullStatus{
				{
					Model:         "qwen2.5:7b",
					ContextLength: 32768,
					CanChat:       true,
					CanPrompt:     true,
				},
			},
		},
		{
			Backend: runtimetypes.Backend{Name: "cloud"},
			PulledModels: []statetype.ModelPullStatus{
				{
					Model:         "qwen2.5:7b",
					ContextLength: 131072,
					CanStream:     true,
				},
				{
					Model:    "text-embedding-3-small",
					CanEmbed: true,
				},
			},
		},
		{
			Backend: runtimetypes.Backend{Name: "broken"},
			Models:  []string{"gpt-5"},
			Error:   "connection refused",
		},
	}

	got := listObservedModels(states)
	if len(got) != 2 {
		t.Fatalf("listObservedModels() len = %d, want 2 (%v)", len(got), got)
	}

	if got[0].Model != "qwen2.5:7b" {
		t.Fatalf("listObservedModels()[0].Model = %q, want qwen2.5:7b", got[0].Model)
	}
	if got[0].ContextLength != 131072 {
		t.Fatalf("listObservedModels()[0].ContextLength = %d, want 131072", got[0].ContextLength)
	}
	if !got[0].CanChat || !got[0].CanPrompt || !got[0].CanStream {
		t.Fatalf("listObservedModels()[0] capabilities = %+v, want merged chat/prompt/stream", got[0])
	}

	if got[1].Model != "text-embedding-3-small" {
		t.Fatalf("listObservedModels()[1].Model = %q, want text-embedding-3-small", got[1].Model)
	}
	if !got[1].CanEmbed {
		t.Fatalf("listObservedModels()[1].CanEmbed = false, want true")
	}
}

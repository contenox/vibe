package setupcheck

// The embedding model gates the optional workspace index; every issue it
// raises is a warning that leaves Ready() true (see addEmbeddingIssues).

import (
	"strings"
	"testing"

	"github.com/contenox/beam/internal/models/runtimestate"
	"github.com/contenox/beam/internal/store/runtimetypes"
)

func embedReadyInput() Input {
	backend := runtimetypes.Backend{ID: "b1", Name: "local", Type: "ollama", BaseURL: "http://localhost:11434"}
	return Input{
		DefaultModel:       "qwen2.5:7b",
		DefaultProvider:    "ollama",
		RegisteredBackends: []runtimetypes.Backend{backend},
		States: []runtimestate.BackendRuntimeState{{
			Backend: backend,
			PulledModels: []runtimestate.ModelPullStatus{
				{Model: "qwen2.5:7b", CanChat: true},
				{Model: "nomic-embed-text", CanEmbed: true},
			},
		}},
	}
}

func findIssue(issues []Issue, code string) *Issue {
	for i := range issues {
		if issues[i].Code == code {
			return &issues[i]
		}
	}
	return nil
}

func TestUnit_Evaluate_UnsetEmbedModelWarnsButNeverBlocks(t *testing.T) {
	res := Evaluate(embedReadyInput())

	iss := findIssue(res.Issues, "embed_model_unset")
	if iss == nil {
		t.Fatalf("expected embed_model_unset issue, got codes %v", issueCodesOf(res.Issues))
	}
	if iss.Severity != "warning" {
		t.Errorf("embed_model_unset severity = %q, want warning", iss.Severity)
	}
	if !strings.Contains(iss.CLICommand, "default-embed-model") {
		t.Errorf("embed_model_unset must name the config key, got %q", iss.CLICommand)
	}
	if !res.Ready() {
		t.Fatalf("an unset embedding model must never make the runtime not-ready; blocking = %v", issueCodesOf(res.BlockingIssues()))
	}
}

func TestUnit_Evaluate_AvailableEmbedModelIsQuiet(t *testing.T) {
	in := embedReadyInput()
	in.DefaultEmbedModel = "nomic-embed-text"
	res := Evaluate(in)

	if iss := findIssue(res.Issues, "embed_model_unset"); iss != nil {
		t.Errorf("a configured embedding model must not warn about being unset")
	}
	if iss := findIssue(res.Issues, "embed_model_not_available"); iss != nil {
		t.Errorf("an available embedding model must not warn: %s", iss.Message)
	}
	if res.DefaultEmbedModel != "nomic-embed-text" {
		t.Errorf("Result.DefaultEmbedModel = %q", res.DefaultEmbedModel)
	}
	if !res.Ready() {
		t.Fatalf("blocking = %v", issueCodesOf(res.BlockingIssues()))
	}
}

func TestUnit_Evaluate_UnembeddableModelWarnsWithAlternatives(t *testing.T) {
	in := embedReadyInput()
	in.DefaultEmbedModel = "not-pulled-embed"
	res := Evaluate(in)

	iss := findIssue(res.Issues, "embed_model_not_available")
	if iss == nil {
		t.Fatalf("expected embed_model_not_available, got codes %v", issueCodesOf(res.Issues))
	}
	if iss.Severity != "warning" {
		t.Errorf("severity = %q, want warning — retrieval is optional", iss.Severity)
	}
	if !strings.Contains(iss.Message, "nomic-embed-text") {
		t.Errorf("the warning must name what IS available, got %q", iss.Message)
	}
	if !res.Ready() {
		t.Fatalf("an unavailable embedding model must never block; blocking = %v", issueCodesOf(res.BlockingIssues()))
	}
}

// TestUnit_Evaluate_NoEmbedCapableModelsWarns pins that "no embedding-capable models at all" is a distinct warning from "wrong name".
func TestUnit_Evaluate_NoEmbedCapableModelsWarns(t *testing.T) {
	in := embedReadyInput()
	in.DefaultEmbedModel = "nomic-embed-text"
	in.States[0].PulledModels = []runtimestate.ModelPullStatus{{Model: "qwen2.5:7b", CanChat: true}}
	res := Evaluate(in)

	iss := findIssue(res.Issues, "embed_model_not_available")
	if iss == nil {
		t.Fatalf("expected embed_model_not_available, got codes %v", issueCodesOf(res.Issues))
	}
	if !strings.Contains(iss.Message, "no embedding-capable models") {
		t.Errorf("message should say the provider exposes none, got %q", iss.Message)
	}
	if !res.Ready() {
		t.Fatalf("blocking = %v", issueCodesOf(res.BlockingIssues()))
	}
}

// TestUnit_Evaluate_EmbedWarningSurvivesEarlyReturn pins that the embedding warning still appends even when evaluateCore returns early.
func TestUnit_Evaluate_EmbedWarningSurvivesEarlyReturn(t *testing.T) {
	res := Evaluate(Input{DefaultModel: "m", DefaultProvider: "ollama"})
	if findIssue(res.Issues, "embed_model_unset") == nil {
		t.Fatalf("expected embed_model_unset even with no backends, got %v", issueCodesOf(res.Issues))
	}
	for _, iss := range res.BlockingIssues() {
		if strings.HasPrefix(iss.Code, "embed_") {
			t.Fatalf("embedding issue %q must never be blocking", iss.Code)
		}
	}
}

func issueCodesOf(issues []Issue) []string {
	out := make([]string, 0, len(issues))
	for _, iss := range issues {
		out = append(out, iss.Code)
	}
	return out
}

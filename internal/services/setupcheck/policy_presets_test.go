package setupcheck

import (
	"strings"
	"testing"

	"github.com/contenox/contenox/internal/models/runtimestate"
	"github.com/contenox/contenox/internal/store/runtimetypes"
)

func readyInput() Input {
	return Input{
		DefaultModel:    "qwen2.5:7b",
		DefaultProvider: "ollama",
		RegisteredBackends: []runtimetypes.Backend{{
			ID: "b1", Name: "local", Type: "ollama", BaseURL: "http://127.0.0.1:11434",
		}},
		States: []runtimestate.BackendRuntimeState{{
			Backend: runtimetypes.Backend{ID: "b1", Name: "local", Type: "ollama", BaseURL: "http://127.0.0.1:11434"},
			PulledModels: []runtimestate.ModelPullStatus{
				{Model: "qwen2.5:7b", CanChat: true},
				{Model: "nomic-embed-text", CanEmbed: true},
			},
		}},
	}
}

// TestUnit_AddStalePolicyPresetIssue_IsAWarningAndNeverBlocks pins that the stale-preset issue names the toolsets and never makes the runtime not ready.
func TestUnit_AddStalePolicyPresetIssue_IsAWarningAndNeverBlocks(t *testing.T) {
	base := Evaluate(readyInput())
	if !base.Ready() {
		t.Fatalf("fixture is not ready before the issue is added: %#v", base.BlockingIssues())
	}

	res := AddStalePolicyPresetIssue(base, []StalePolicyPreset{{
		Name:     "hitl-policy-default.json",
		Path:     "/home/u/.contenox/hitl-policy-default.json",
		Toolsets: []string{"goja", "local_shell", "webtools", "workspace"},
		Effect:   "every call stops for approval",
	}}, "contenox init --refresh-policies")

	var found Issue
	for _, iss := range res.Issues {
		if iss.Code == StalePolicyPresetsCode {
			found = iss
		}
	}
	if found.Code == "" {
		t.Fatalf("no %s issue was added: %#v", StalePolicyPresetsCode, res.Issues)
	}
	if found.Severity != "warning" {
		t.Fatalf("severity = %q, want warning (a stale envelope never blocks)", found.Severity)
	}
	if found.Category != CategoryPolicy {
		t.Fatalf("category = %q, want %q", found.Category, CategoryPolicy)
	}
	for _, want := range []string{"/home/u/.contenox/hitl-policy-default.json", "goja", "local_shell", "webtools", "workspace", "stops for approval"} {
		if !strings.Contains(found.Message, want) {
			t.Fatalf("message does not name %q: %s", want, found.Message)
		}
	}
	if found.CLICommand != "contenox init --refresh-policies" {
		t.Fatalf("cliCommand = %q, want the refresh verb", found.CLICommand)
	}

	for _, iss := range res.BlockingIssues() {
		if iss.Code == StalePolicyPresetsCode {
			t.Fatalf("the stale-policy warning reached BlockingIssues: %#v", iss)
		}
	}
	if !res.Ready() {
		t.Fatalf("a stale policy preset made the runtime not ready: %#v", res.BlockingIssues())
	}
}

// TestUnit_AddStalePolicyPresetIssue_NamesPathAndDefaultActionFallThrough pins the message contract: every stale file appears with its full path, its effect reads as that file's default_action fall-through, and tool visibility is stated as unaffected.
func TestUnit_AddStalePolicyPresetIssue_NamesPathAndDefaultActionFallThrough(t *testing.T) {
	res := AddStalePolicyPresetIssue(Evaluate(readyInput()), []StalePolicyPreset{
		{
			Name:     "hitl-policy-default.json",
			Path:     "/w/.contenox/hitl-policy-default.json",
			Toolsets: []string{"git", "goja"},
			Effect:   "every call stops for approval",
		},
		{
			Name:     "hitl-policy-strict.json",
			Path:     "/home/u/.contenox/hitl-policy-strict.json",
			Toolsets: []string{"git"},
			Effect:   "every call is denied",
		},
	}, "contenox init --refresh-policies")

	var found Issue
	for _, iss := range res.Issues {
		if iss.Code == StalePolicyPresetsCode {
			found = iss
		}
	}
	if found.Code == "" {
		t.Fatalf("no %s issue was added: %#v", StalePolicyPresetsCode, res.Issues)
	}
	for _, want := range []string{
		// Path + toolsets + that file's own fall-through, per file.
		"/w/.contenox/hitl-policy-default.json predates toolsets git, goja — calls to them fall to this file's default_action (every call stops for approval)",
		"/home/u/.contenox/hitl-policy-strict.json predates toolsets git — calls to them fall to this file's default_action (every call is denied)",
		// The rule list never gates availability; the message must say so.
		"The tools stay visible to the model",
		"never overwritten automatically",
	} {
		if !strings.Contains(found.Message, want) {
			t.Fatalf("message does not contain %q: %s", want, found.Message)
		}
	}
	if strings.Contains(found.Message, "\n") {
		t.Fatalf("message must stay a single line for the doctor bullet renderer: %q", found.Message)
	}
}

// TestUnit_AddStalePolicyPresetIssue_SaysNothingWhenNothingIsStale pins that no stale presets (or no named toolsets) adds no issue.
func TestUnit_AddStalePolicyPresetIssue_SaysNothingWhenNothingIsStale(t *testing.T) {
	base := Evaluate(readyInput())
	before := len(base.Issues)

	for _, stale := range [][]StalePolicyPreset{
		nil,
		{},
		{{Name: "hitl-policy-default.json"}}, // no toolsets: nothing to say
	} {
		res := AddStalePolicyPresetIssue(base, stale, "contenox init --refresh-policies")
		for _, iss := range res.Issues {
			if iss.Code == StalePolicyPresetsCode {
				t.Fatalf("issue raised for %#v", stale)
			}
		}
		if len(res.Issues) != before {
			t.Fatalf("issue count changed: %d -> %d", before, len(res.Issues))
		}
	}
}

// TestUnit_AddStalePolicyPresetIssue_DoesNotMutateTheCallersIssues pins the copy-on-append: the caller's Issues slice is never written into.
func TestUnit_AddStalePolicyPresetIssue_DoesNotMutateTheCallersIssues(t *testing.T) {
	base := Evaluate(readyInput())
	baseIssues := append([]Issue(nil), base.Issues...)

	_ = AddStalePolicyPresetIssue(base, []StalePolicyPreset{{
		Name: "hitl-policy-default.json", Toolsets: []string{"workspace"},
	}}, "contenox init --refresh-policies")

	if len(base.Issues) != len(baseIssues) {
		t.Fatalf("caller's issue slice grew: %d -> %d", len(baseIssues), len(base.Issues))
	}
	for i := range baseIssues {
		if base.Issues[i].Code != baseIssues[i].Code {
			t.Fatalf("caller's issue %d changed: %q -> %q", i, baseIssues[i].Code, base.Issues[i].Code)
		}
	}
}

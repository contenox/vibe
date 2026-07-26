package setupcheck

import "testing"

func TestOverlayEffectiveDefaults(t *testing.T) {
	missingModel := Issue{Code: "missing_default_model", Severity: "error"}
	missingProvider := Issue{Code: "missing_default_provider", Severity: "error"}
	unrelated := Issue{Code: "no_backends", Severity: "warning"}

	t.Run("fills empty model and drops its blocking issue", func(t *testing.T) {
		res := Result{Issues: []Issue{missingModel, unrelated}}
		got := OverlayEffectiveDefaults(res, "qwen2.5:7b", "")
		if got.DefaultModel != "qwen2.5:7b" {
			t.Errorf("DefaultModel = %q, want qwen2.5:7b", got.DefaultModel)
		}
		if hasIssue(got, "missing_default_model") {
			t.Error("missing_default_model should have been dropped")
		}
		if !hasIssue(got, "no_backends") {
			t.Error("unrelated issue should be preserved")
		}
	})

	t.Run("fills empty provider and drops its blocking issue", func(t *testing.T) {
		res := Result{Issues: []Issue{missingProvider}}
		got := OverlayEffectiveDefaults(res, "", "ollama")
		if got.DefaultProvider != "ollama" {
			t.Errorf("DefaultProvider = %q, want ollama", got.DefaultProvider)
		}
		if hasIssue(got, "missing_default_provider") {
			t.Error("missing_default_provider should have been dropped")
		}
	})

	t.Run("model override with unset provider leaves provider issue blocking", func(t *testing.T) {
		res := Result{Issues: []Issue{missingModel, missingProvider}}
		got := OverlayEffectiveDefaults(res, "qwen2.5:7b", "")
		if got.Ready() {
			t.Error("still not ready: provider is unset")
		}
		if !hasIssue(got, "missing_default_provider") {
			t.Error("missing_default_provider must remain")
		}
	})

	t.Run("never overwrites a default already set by config", func(t *testing.T) {
		res := Result{DefaultModel: "persisted", DefaultProvider: "ollama"}
		got := OverlayEffectiveDefaults(res, "flagmodel", "openai")
		if got.DefaultModel != "persisted" || got.DefaultProvider != "ollama" {
			t.Errorf("persisted defaults overwritten: model=%q provider=%q", got.DefaultModel, got.DefaultProvider)
		}
	})

	t.Run("empty overrides are a no-op and preserve issues", func(t *testing.T) {
		res := Result{Issues: []Issue{missingModel, missingProvider}}
		got := OverlayEffectiveDefaults(res, "  ", "")
		if !hasIssue(got, "missing_default_model") || !hasIssue(got, "missing_default_provider") {
			t.Error("empty overrides should not drop any issue")
		}
	})

	t.Run("overlay does not mutate the caller's Issues", func(t *testing.T) {
		res := Result{Issues: []Issue{missingModel, unrelated}}
		_ = OverlayEffectiveDefaults(res, "qwen2.5:7b", "")
		if len(res.Issues) != 2 || res.Issues[0].Code != "missing_default_model" {
			t.Errorf("caller's Issues were mutated: %+v", res.Issues)
		}
	})

	t.Run("both defaults credited makes an otherwise-blocked result ready", func(t *testing.T) {
		res := Result{Issues: []Issue{missingModel, missingProvider}}
		got := OverlayEffectiveDefaults(res, "qwen2.5:7b", "ollama")
		if !got.Ready() {
			t.Errorf("expected ready after crediting both defaults; blocking=%v", got.BlockingIssues())
		}
	})
}

func hasIssue(r Result, code string) bool {
	for _, iss := range r.Issues {
		if iss.Code == code {
			return true
		}
	}
	return false
}

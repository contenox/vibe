package setupcheck

import (
	"strings"
	"testing"
)

func TestResultSummary(t *testing.T) {
	ready := Result{DefaultModel: "qwen2.5:7b", DefaultProvider: "ollama", BackendCount: 1, ReachableBackendCount: 1}
	s := ready.Summary()
	if !strings.Contains(s, "✓ Ready") {
		t.Errorf("ready summary missing ready marker:\n%s", s)
	}
	if !strings.Contains(s, "1/1 reachable") {
		t.Errorf("ready summary missing backend count:\n%s", s)
	}

	notReady := Result{
		DefaultProvider: "openai",
		BackendCount:    1,
		Issues: []Issue{{
			Code: "default_provider_auth_failed", Severity: "error",
			Message: "credentials rejected", CLICommand: "contenox backend add ...",
		}},
	}
	ns := notReady.Summary()
	if !strings.Contains(ns, "Not ready") {
		t.Errorf("not-ready summary missing marker:\n%s", ns)
	}
	if !strings.Contains(ns, "credentials rejected") || !strings.Contains(ns, "Try: contenox backend add") {
		t.Errorf("not-ready summary missing issue/fix:\n%s", ns)
	}
	if !strings.Contains(ns, "contenox doctor") {
		t.Errorf("not-ready summary missing doctor pointer:\n%s", ns)
	}
}

func TestResultReady(t *testing.T) {
	cases := []struct {
		name         string
		issues       []Issue
		wantReady    bool
		wantBlocking int
	}{
		{name: "no issues is ready", issues: nil, wantReady: true, wantBlocking: 0},
		{
			name:      "non-blocking warning is still ready",
			issues:    []Issue{{Code: "some_warning", Severity: "warning"}},
			wantReady: true, wantBlocking: 0,
		},
		{
			name:         "error-severity issue blocks",
			issues:       []Issue{{Code: "missing_default_model", Severity: "error"}},
			wantReady:    false,
			wantBlocking: 1,
		},
		{
			name:         "no_backends warning is blocking despite warning severity",
			issues:       []Issue{{Code: "no_backends", Severity: "warning"}},
			wantReady:    false,
			wantBlocking: 1,
		},
		{
			name: "counts only blocking issues",
			issues: []Issue{
				{Code: "no_backends", Severity: "warning"},
				{Code: "cosmetic", Severity: "warning"},
				{Code: "default_model_not_available", Severity: "error"},
			},
			wantReady:    false,
			wantBlocking: 2,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := Result{Issues: tc.issues}
			if got := r.Ready(); got != tc.wantReady {
				t.Errorf("Ready() = %v, want %v", got, tc.wantReady)
			}
			if got := len(r.BlockingIssues()); got != tc.wantBlocking {
				t.Errorf("len(BlockingIssues()) = %d, want %d", got, tc.wantBlocking)
			}
		})
	}
}

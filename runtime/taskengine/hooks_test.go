package taskengine_test

import (
	"context"
	"sort"
	"testing"

	"github.com/contenox/contenox/runtime/taskengine"
)

// resolveHookNames is tested indirectly via the exported behaviour through
// MacroEnv and SimpleEnv, but we also exercise it directly by constructing a
// minimal HookProvider stub.

func sortedNames(names []string) []string {
	cp := append([]string(nil), names...)
	sort.Strings(cp)
	return cp
}

func TestResolveHookNames_Nil_ReturnsAll(t *testing.T) {
	repo := stubRepo()
	names, err := taskengine.ExportedResolveHookNames(context.Background(), nil, repo)
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 3 {
		t.Errorf("nil allowlist: expected 3, got %d: %v", len(names), names)
	}
}

func TestResolveHookNames_Empty_ReturnsNothing(t *testing.T) {
	repo := stubRepo()
	names, err := taskengine.ExportedResolveHookNames(context.Background(), []string{}, repo)
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 0 {
		t.Errorf("empty allowlist: expected 0, got %d: %v", len(names), names)
	}
}

func TestResolveHookNames_Star_ReturnsAll(t *testing.T) {
	repo := stubRepo()
	names, err := taskengine.ExportedResolveHookNames(context.Background(), []string{"*"}, repo)
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 3 {
		t.Errorf("[*]: expected 3, got %d: %v", len(names), names)
	}
}

func TestResolveHookNames_Exact_Match(t *testing.T) {
	repo := stubRepo()
	names, err := taskengine.ExportedResolveHookNames(context.Background(), []string{"hook_a"}, repo)
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 1 || names[0] != "hook_a" {
		t.Errorf("[hook_a]: expected [hook_a], got %v", names)
	}
}

func TestResolveHookNames_Exact_Miss(t *testing.T) {
	repo := stubRepo()
	names, err := taskengine.ExportedResolveHookNames(context.Background(), []string{"unknown"}, repo)
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 0 {
		t.Errorf("[unknown]: expected empty, got %v", names)
	}
}

func TestResolveHookNames_StarExclude_RemovesEntry(t *testing.T) {
	repo := stubRepo()
	names, err := taskengine.ExportedResolveHookNames(context.Background(), []string{"*", "!hook_b"}, repo)
	if err != nil {
		t.Fatal(err)
	}
	got := sortedNames(names)
	if len(got) != 2 || got[0] != "hook_a" || got[1] != "hook_c" {
		t.Errorf("[*, !hook_b]: expected [hook_a hook_c], got %v", got)
	}
}

func TestResolveHookNames_StarExcludeMiss_ReturnsAll(t *testing.T) {
	repo := stubRepo()
	names, err := taskengine.ExportedResolveHookNames(context.Background(), []string{"*", "!hook_x"}, repo)
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 3 {
		t.Errorf("[*, !hook_x]: expected 3, got %d: %v", len(names), names)
	}
}

// Runtime allowlist intersection (WithRuntimeHookAllowlist) — caller can further
// restrict but never expand what a task allowlist permits.

func TestResolveHookNames_Runtime_Absent_TaskUnchanged(t *testing.T) {
	repo := stubRepo()
	names, err := taskengine.ExportedResolveHookNames(context.Background(), []string{"*"}, repo)
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 3 {
		t.Errorf("runtime absent: expected 3, got %d: %v", len(names), names)
	}
}

func TestResolveHookNames_Runtime_StarExclude_RemovesEntry(t *testing.T) {
	repo := stubRepo()
	ctx := taskengine.WithRuntimeHookAllowlist(context.Background(), []string{"*", "!hook_b"})
	names, err := taskengine.ExportedResolveHookNames(ctx, []string{"*"}, repo)
	if err != nil {
		t.Fatal(err)
	}
	got := sortedNames(names)
	if len(got) != 2 || got[0] != "hook_a" || got[1] != "hook_c" {
		t.Errorf("runtime [*, !hook_b] ∩ task [*]: expected [hook_a hook_c], got %v", got)
	}
}

func TestResolveHookNames_Runtime_Empty_DeniesAll(t *testing.T) {
	repo := stubRepo()
	ctx := taskengine.WithRuntimeHookAllowlist(context.Background(), []string{})
	names, err := taskengine.ExportedResolveHookNames(ctx, []string{"*"}, repo)
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 0 {
		t.Errorf("runtime [] ∩ task [*]: expected empty, got %v", names)
	}
}

func TestResolveHookNames_Runtime_NilSlice_NoRestriction(t *testing.T) {
	repo := stubRepo()
	ctx := taskengine.WithRuntimeHookAllowlist(context.Background(), nil)
	names, err := taskengine.ExportedResolveHookNames(ctx, []string{"hook_a"}, repo)
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 1 || names[0] != "hook_a" {
		t.Errorf("runtime nil ∩ task [hook_a]: expected [hook_a], got %v", names)
	}
}

func TestResolveHookNames_Runtime_CannotExpandTaskAllowlist(t *testing.T) {
	repo := stubRepo()
	ctx := taskengine.WithRuntimeHookAllowlist(context.Background(), []string{"*"})
	names, err := taskengine.ExportedResolveHookNames(ctx, []string{"hook_a"}, repo)
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 1 || names[0] != "hook_a" {
		t.Errorf("runtime [*] ∩ task [hook_a]: expected [hook_a], got %v", names)
	}
}

func TestResolveHookNames_Runtime_StricterWins(t *testing.T) {
	repo := stubRepo()
	ctx := taskengine.WithRuntimeHookAllowlist(context.Background(), []string{"hook_a"})
	names, err := taskengine.ExportedResolveHookNames(ctx, []string{"*"}, repo)
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 1 || names[0] != "hook_a" {
		t.Errorf("runtime [hook_a] ∩ task [*]: expected [hook_a], got %v", names)
	}
}

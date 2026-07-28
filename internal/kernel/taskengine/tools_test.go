package taskengine_test

import (
	"context"
	"sort"
	"testing"

	"github.com/contenox/contenox/internal/kernel/taskengine"
)

func sortedNames(names []string) []string {
	cp := append([]string(nil), names...)
	sort.Strings(cp)
	return cp
}

func TestUnit_ResolveToolsNames_Nil_ReturnsEmpty(t *testing.T) {
	repo := stubRepo()
	names, err := taskengine.ExportedResolveToolsNames(context.Background(), nil, repo)
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 0 {
		t.Errorf("nil allowlist: expected 0 (no tools), got %d: %v", len(names), names)
	}
}

func TestUnit_ResolveToolsNames_Empty_ReturnsNothing(t *testing.T) {
	repo := stubRepo()
	names, err := taskengine.ExportedResolveToolsNames(context.Background(), []string{}, repo)
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 0 {
		t.Errorf("empty allowlist: expected 0, got %d: %v", len(names), names)
	}
}

func TestUnit_ResolveToolsNames_Star_ReturnsAll(t *testing.T) {
	repo := stubRepo()
	names, err := taskengine.ExportedResolveToolsNames(context.Background(), []string{"*"}, repo)
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 3 {
		t.Errorf("[*]: expected 3, got %d: %v", len(names), names)
	}
}

func TestUnit_ResolveToolsNames_Exact_Match(t *testing.T) {
	repo := stubRepo()
	names, err := taskengine.ExportedResolveToolsNames(context.Background(), []string{"tools_a"}, repo)
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 1 || names[0] != "tools_a" {
		t.Errorf("[tools_a]: expected [tools_a], got %v", names)
	}
}

func TestUnit_ResolveToolsNames_Exact_Miss(t *testing.T) {
	repo := stubRepo()
	names, err := taskengine.ExportedResolveToolsNames(context.Background(), []string{"unknown"}, repo)
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 0 {
		t.Errorf("[unknown]: expected empty, got %v", names)
	}
}

func TestUnit_ResolveToolsNames_StarExclude_RemovesEntry(t *testing.T) {
	repo := stubRepo()
	names, err := taskengine.ExportedResolveToolsNames(context.Background(), []string{"*", "!tools_b"}, repo)
	if err != nil {
		t.Fatal(err)
	}
	got := sortedNames(names)
	if len(got) != 2 || got[0] != "tools_a" || got[1] != "tools_c" {
		t.Errorf("[*, !tools_b]: expected [tools_a tools_c], got %v", got)
	}
}

func TestUnit_ResolveToolsNames_StarExcludeMiss_ReturnsAll(t *testing.T) {
	repo := stubRepo()
	names, err := taskengine.ExportedResolveToolsNames(context.Background(), []string{"*", "!tools_x"}, repo)
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 3 {
		t.Errorf("[*, !tools_x]: expected 3, got %d: %v", len(names), names)
	}
}

// Runtime allowlist intersection (WithRuntimeToolsAllowlist) — caller can further
// restrict but never expand what a task allowlist permits.

func TestUnit_ResolveToolsNames_Runtime_Absent_TaskUnchanged(t *testing.T) {
	repo := stubRepo()
	names, err := taskengine.ExportedResolveToolsNames(context.Background(), []string{"*"}, repo)
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 3 {
		t.Errorf("runtime absent: expected 3, got %d: %v", len(names), names)
	}
}

func TestUnit_ResolveToolsNames_Runtime_StarExclude_RemovesEntry(t *testing.T) {
	repo := stubRepo()
	ctx := taskengine.WithRuntimeToolsAllowlist(context.Background(), []string{"*", "!tools_b"})
	names, err := taskengine.ExportedResolveToolsNames(ctx, []string{"*"}, repo)
	if err != nil {
		t.Fatal(err)
	}
	got := sortedNames(names)
	if len(got) != 2 || got[0] != "tools_a" || got[1] != "tools_c" {
		t.Errorf("runtime [*, !tools_b] ∩ task [*]: expected [tools_a tools_c], got %v", got)
	}
}

func TestUnit_ResolveToolsNames_Runtime_Empty_DeniesAll(t *testing.T) {
	repo := stubRepo()
	ctx := taskengine.WithRuntimeToolsAllowlist(context.Background(), []string{})
	names, err := taskengine.ExportedResolveToolsNames(ctx, []string{"*"}, repo)
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 0 {
		t.Errorf("runtime [] ∩ task [*]: expected empty, got %v", names)
	}
}

func TestUnit_ResolveToolsNames_Runtime_NilSlice_NoRestriction(t *testing.T) {
	repo := stubRepo()
	ctx := taskengine.WithRuntimeToolsAllowlist(context.Background(), nil)
	names, err := taskengine.ExportedResolveToolsNames(ctx, []string{"tools_a"}, repo)
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 1 || names[0] != "tools_a" {
		t.Errorf("runtime nil ∩ task [tools_a]: expected [tools_a], got %v", names)
	}
}

func TestUnit_ResolveToolsNames_Runtime_CannotExpandTaskAllowlist(t *testing.T) {
	repo := stubRepo()
	ctx := taskengine.WithRuntimeToolsAllowlist(context.Background(), []string{"*"})
	names, err := taskengine.ExportedResolveToolsNames(ctx, []string{"tools_a"}, repo)
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 1 || names[0] != "tools_a" {
		t.Errorf("runtime [*] ∩ task [tools_a]: expected [tools_a], got %v", names)
	}
}

func TestUnit_ResolveToolsNames_Runtime_StricterWins(t *testing.T) {
	repo := stubRepo()
	ctx := taskengine.WithRuntimeToolsAllowlist(context.Background(), []string{"tools_a"})
	names, err := taskengine.ExportedResolveToolsNames(ctx, []string{"*"}, repo)
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 1 || names[0] != "tools_a" {
		t.Errorf("runtime [tools_a] ∩ task [*]: expected [tools_a], got %v", names)
	}
}

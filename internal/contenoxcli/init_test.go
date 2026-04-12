package contenoxcli

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/contenox/contenox/taskengine"
)

func TestRunInit_writesAllDefaultChainsIncludingPlan(t *testing.T) {
	t.Parallel()
	dir, err := os.MkdirTemp("", "contenox-init-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	out := &strings.Builder{}
	errOut := &strings.Builder{}
	if err := RunInit(out, errOut, true, "ollama", dir); err != nil {
		t.Fatal(err)
	}

	expected := []string{
		"default-chain.json",
		"default-run-chain.json",
		"chain-planner.json",
		"chain-step-executor.json",
	}
	for _, name := range expected {
		p := filepath.Join(dir, name)
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("missing %s: %v", name, err)
		}
	}

	plannerPath := filepath.Join(dir, "chain-planner.json")
	execPath := filepath.Join(dir, "chain-step-executor.json")
	plannerData, err := os.ReadFile(plannerPath)
	if err != nil {
		t.Fatal(err)
	}
	execData, err := os.ReadFile(execPath)
	if err != nil {
		t.Fatal(err)
	}
	var planner, executor taskengine.TaskChainDefinition
	if err := json.Unmarshal(plannerData, &planner); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(execData, &executor); err != nil {
		t.Fatal(err)
	}
	if err := validatePlannerChain(&planner, plannerPath); err != nil {
		t.Fatal(err)
	}
	if err := validateExecutorChain(&executor, execPath); err != nil {
		t.Fatal(err)
	}
}

func TestRunInit_respectsForce_skipExistingPlanChains(t *testing.T) {
	t.Parallel()
	dir, err := os.MkdirTemp("", "contenox-init-skip-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	if err := RunInit(io.Discard, io.Discard, true, "ollama", dir); err != nil {
		t.Fatal(err)
	}
	// Touch planner to a sentinel so we can detect overwrite
	plannerPath := filepath.Join(dir, "chain-planner.json")
	if err := os.WriteFile(plannerPath, []byte("sentinel"), 0644); err != nil {
		t.Fatal(err)
	}

	if err := RunInit(io.Discard, io.Discard, false, "ollama", dir); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(plannerPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "sentinel" {
		t.Fatalf("expected planner left unchanged without --force, got %q", string(b))
	}
}

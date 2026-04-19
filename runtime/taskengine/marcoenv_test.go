package taskengine_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/contenox/contenox/runtime/taskengine"
	"github.com/getkin/kin-openapi/openapi3"
)

// stubHookRepo is a minimal HookRepo for macro expansion tests.
type stubHookRepo struct {
	names map[string][]taskengine.Tool
}

func (s *stubHookRepo) Supports(_ context.Context) ([]string, error) {
	out := make([]string, 0, len(s.names))
	for n := range s.names {
		out = append(out, n)
	}
	return out, nil
}

func (s *stubHookRepo) GetSchemasForSupportedHooks(_ context.Context) (map[string]*openapi3.T, error) {
	return nil, nil
}

func (s *stubHookRepo) GetToolsForHookByName(_ context.Context, name string) ([]taskengine.Tool, error) {
	tools, ok := s.names[name]
	if !ok {
		return nil, taskengine.ErrHookNotFound
	}
	return tools, nil
}

func (s *stubHookRepo) Exec(_ context.Context, _ time.Time, _ any, _ bool, _ *taskengine.HookCall) (any, taskengine.DataType, error) {
	return nil, taskengine.DataTypeAny, nil
}

func tool(name string) taskengine.Tool {
	return taskengine.Tool{Type: "function", Function: taskengine.FunctionTool{Name: name}}
}

func newMacroChain(template string, hooks []string) *taskengine.TaskChainDefinition {
	cfg := &taskengine.LLMExecutionConfig{
		Model:  "test",
		Hooks:  hooks,
	}
	return &taskengine.TaskChainDefinition{
		ID: "test-chain",
		Tasks: []taskengine.TaskDefinition{
			{
				ID:             "task1",
				Handler:        taskengine.HandlePromptToString,
				PromptTemplate: template,
				ExecuteConfig:  cfg,
				Transition:     taskengine.TaskTransition{Branches: []taskengine.TransitionBranch{{Operator: "default", Goto: "end"}}},
			},
		},
	}
}

func runMacroExpand(t *testing.T, repo taskengine.HookRepo, sysInstruction string, hooks []string) string {
	t.Helper()
	// We only test macro expansion; wrap a noop inner executor.
	inner := &noopEnv{}
	env, err := taskengine.NewMacroEnv(inner, repo)
	if err != nil {
		t.Fatalf("NewMacroEnv: %v", err)
	}
	chain := newMacroChain(sysInstruction, hooks)
	// ExecEnv expands macros then delegates to noopEnv which returns the expanded system_instruction.
	raw, _, _, err := env.ExecEnv(context.Background(), chain, "", taskengine.DataTypeString)
	if err != nil {
		t.Fatalf("ExecEnv: %v", err)
	}
	s, ok := raw.(string)
	if !ok {
		t.Fatalf("expected string output, got %T", raw)
	}
	return s
}

// noopEnv captures the expanded system_instruction from the first task and returns it.
type noopEnv struct{}

func (n *noopEnv) ExecEnv(_ context.Context, chain *taskengine.TaskChainDefinition, input any, _ taskengine.DataType) (any, taskengine.DataType, []taskengine.CapturedStateUnit, error) {
	if len(chain.Tasks) > 0 {
		return chain.Tasks[0].PromptTemplate, taskengine.DataTypeString, nil, nil
	}
	return input, taskengine.DataTypeString, nil, nil
}

func stubRepo() *stubHookRepo {
	return &stubHookRepo{names: map[string][]taskengine.Tool{
		"hook_a": {tool("tool_a1"), tool("tool_a2")},
		"hook_b": {tool("tool_b1")},
		"hook_c": {tool("tool_c1")},
	}}
}

// ── hookservice:hooks ──────────────────────────────────────────────────────────

func TestMacroEnv_Hooks_NoAllowlist(t *testing.T) {
	// nil allowlist = field absent = all hooks (backward compat)
	out := runMacroExpand(t, stubRepo(), "{{hookservice:hooks}}", nil)
	var names []string
	if err := json.Unmarshal([]byte(out), &names); err != nil {
		t.Fatalf("not JSON: %v — got: %s", err, out)
	}
	if len(names) != 3 {
		t.Errorf("expected 3 hooks, got %d: %v", len(names), names)
	}
}

func TestMacroEnv_Hooks_StarAllowlist(t *testing.T) {
	// ["*"] = explicit all
	out := runMacroExpand(t, stubRepo(), "{{hookservice:hooks}}", []string{"*"})
	var names []string
	if err := json.Unmarshal([]byte(out), &names); err != nil {
		t.Fatalf("not JSON: %v — got: %s", err, out)
	}
	if len(names) != 3 {
		t.Errorf("expected 3 hooks with [*], got %d: %v", len(names), names)
	}
}

func TestMacroEnv_Hooks_EmptyAllowlist(t *testing.T) {
	// [] = explicitly no hooks
	out := runMacroExpand(t, stubRepo(), "{{hookservice:hooks}}", []string{})
	var names []string
	if err := json.Unmarshal([]byte(out), &names); err != nil {
		t.Fatalf("not JSON: %v — got: %s", err, out)
	}
	if len(names) != 0 {
		t.Errorf("empty allowlist: expected 0 hooks, got %d: %v", len(names), names)
	}
}

func TestMacroEnv_Hooks_WithAllowlist(t *testing.T) {
	out := runMacroExpand(t, stubRepo(), "{{hookservice:hooks}}", []string{"hook_a"})
	var names []string
	if err := json.Unmarshal([]byte(out), &names); err != nil {
		t.Fatalf("not JSON: %v — got: %s", err, out)
	}
	if len(names) != 1 || names[0] != "hook_a" {
		t.Errorf("expected [hook_a], got %v", names)
	}
}

func TestMacroEnv_Hooks_AllowlistMiss(t *testing.T) {
	out := runMacroExpand(t, stubRepo(), "{{hookservice:hooks}}", []string{"hook_x"})
	var names []string
	if err := json.Unmarshal([]byte(out), &names); err != nil {
		t.Fatalf("not JSON: %v — got: %s", err, out)
	}
	if len(names) != 0 {
		t.Errorf("expected empty, got %v", names)
	}
}

// ── hookservice:list ───────────────────────────────────────────────────────────

func TestMacroEnv_List_WithAllowlist(t *testing.T) {
	out := runMacroExpand(t, stubRepo(), "{{hookservice:list}}", []string{"hook_a"})
	var m map[string][]string
	if err := json.Unmarshal([]byte(out), &m); err != nil {
		t.Fatalf("not JSON: %v — got: %s", err, out)
	}
	if _, ok := m["hook_a"]; !ok {
		t.Errorf("hook_a should be in map, got keys: %v", keys(m))
	}
	if _, ok := m["hook_b"]; ok {
		t.Errorf("hook_b should NOT be in map")
	}
}

// ── hookservice:tools ──────────────────────────────────────────────────────────

func TestMacroEnv_Tools_Allowed(t *testing.T) {
	out := runMacroExpand(t, stubRepo(), "{{hookservice:tools hook_a}}", []string{"hook_a"})
	var names []string
	if err := json.Unmarshal([]byte(out), &names); err != nil {
		t.Fatalf("not JSON: %v — got: %s", err, out)
	}
	if len(names) != 2 {
		t.Errorf("expected 2 tools, got %v", names)
	}
}

func TestMacroEnv_Tools_NotAllowed(t *testing.T) {
	out := runMacroExpand(t, stubRepo(), "{{hookservice:tools hook_b}}", []string{"hook_a"})
	// hook_b is not in allowlist → should return empty array
	var names []string
	if err := json.Unmarshal([]byte(out), &names); err != nil {
		t.Fatalf("not JSON: %v — got: %s", err, out)
	}
	if len(names) != 0 {
		t.Errorf("expected empty for disallowed hook, got %v", names)
	}
}

func TestMacroEnv_Tools_NoAllowlist_Allowed(t *testing.T) {
	// nil = no allowlist = all hooks accessible
	out := runMacroExpand(t, stubRepo(), "{{hookservice:tools hook_b}}", nil)
	if strings.Contains(out, "tool_b1") {
		return // good
	}
	t.Errorf("expected tool_b1 when nil allowlist, got: %s", out)
}

func keys(m map[string][]string) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	return ks
}

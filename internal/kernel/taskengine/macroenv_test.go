package taskengine_test

import (
	"context"
	"encoding/json"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/contenox/contenox/internal/kernel/taskengine"
	"github.com/contenox/contenox/internal/libtracker"
	"github.com/getkin/kin-openapi/openapi3"
	"gopkg.in/yaml.v3"
)

// stubToolsRepo is a minimal ToolsRepo for macro expansion tests.
type stubToolsRepo struct {
	names map[string][]taskengine.Tool
}

func (s *stubToolsRepo) Supports(_ context.Context) ([]string, error) {
	out := make([]string, 0, len(s.names))
	for n := range s.names {
		out = append(out, n)
	}
	return out, nil
}

func (s *stubToolsRepo) GetSchemasForSupportedTools(_ context.Context) (map[string]*openapi3.T, error) {
	return nil, nil
}

func (s *stubToolsRepo) GetToolsForToolsByName(_ context.Context, name string) ([]taskengine.Tool, error) {
	tools, ok := s.names[name]
	if !ok {
		return nil, taskengine.ErrToolsNotFound
	}
	return tools, nil
}

func (s *stubToolsRepo) Exec(_ context.Context, _ time.Time, _ any, _ bool, _ *taskengine.ToolsCall) (any, taskengine.DataType, error) {
	return nil, taskengine.DataTypeAny, nil
}

func tool(name string) taskengine.Tool {
	return taskengine.Tool{Type: "function", Function: taskengine.FunctionTool{Name: name}}
}

func newMacroChain(template string, tools []string) *taskengine.TaskChainDefinition {
	cfg := &taskengine.LLMExecutionConfig{
		Model: "test",
		Tools: tools,
	}
	return &taskengine.TaskChainDefinition{
		ID: "test-chain",
		Tasks: []taskengine.TaskDefinition{
			{
				ID:             "task1",
				Handler:        taskengine.HandleNoop,
				PromptTemplate: template,
				ExecuteConfig:  cfg,
				Transition:     taskengine.TaskTransition{Branches: []taskengine.TransitionBranch{{Operator: "default", Goto: "end"}}},
			},
		},
	}
}

func runMacroExpand(t *testing.T, repo taskengine.ToolsRepo, sysInstruction string, tools []string) string {
	t.Helper()
	// We only test macro expansion; wrap a noop inner executor.
	inner := &noopEnv{}
	env, err := taskengine.NewMacroEnv(inner, repo)
	if err != nil {
		t.Fatalf("NewMacroEnv: %v", err)
	}
	chain := newMacroChain(sysInstruction, tools)
	// ExecEnv expands macros then delegates to noopEnv which returns the expanded system_instruction.
	raw, _, _, err := env.ExecEnv(libtracker.WithNewRequestID(context.Background()), chain, "", taskengine.DataTypeString)
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

func stubRepo() *stubToolsRepo {
	return &stubToolsRepo{names: map[string][]taskengine.Tool{
		"tools_a": {tool("tool_a1"), tool("tool_a2")},
		"tools_b": {tool("tool_b1")},
		"tools_c": {tool("tool_c1")},
	}}
}

func TestUnit_MacroEnv_Tools_NoAllowlist(t *testing.T) {
	// nil allowlist = no tools exposed; use ["*"] to expose all tools
	out := runMacroExpand(t, stubRepo(), "{{toolservice:tools}}", nil)
	var names []string
	if err := json.Unmarshal([]byte(out), &names); err != nil {
		t.Fatalf("not JSON: %v — got: %s", err, out)
	}
	if len(names) != 0 {
		t.Errorf("nil allowlist: expected 0 tools (no tools exposed), got %d: %v", len(names), names)
	}
}

func TestUnit_MacroEnv_Tools_StarAllowlist(t *testing.T) {
	// ["*"] = explicit all
	out := runMacroExpand(t, stubRepo(), "{{toolservice:tools}}", []string{"*"})
	var names []string
	if err := json.Unmarshal([]byte(out), &names); err != nil {
		t.Fatalf("not JSON: %v — got: %s", err, out)
	}
	if len(names) != 3 {
		t.Errorf("expected 3 tools with [*], got %d: %v", len(names), names)
	}
}

func TestUnit_MacroEnv_Tools_EmptyAllowlist(t *testing.T) {
	// [] = explicitly no tools
	out := runMacroExpand(t, stubRepo(), "{{toolservice:tools}}", []string{})
	var names []string
	if err := json.Unmarshal([]byte(out), &names); err != nil {
		t.Fatalf("not JSON: %v — got: %s", err, out)
	}
	if len(names) != 0 {
		t.Errorf("empty allowlist: expected 0 tools, got %d: %v", len(names), names)
	}
}

func TestUnit_MacroEnv_Tools_WithAllowlist(t *testing.T) {
	out := runMacroExpand(t, stubRepo(), "{{toolservice:tools}}", []string{"tools_a"})
	var names []string
	if err := json.Unmarshal([]byte(out), &names); err != nil {
		t.Fatalf("not JSON: %v — got: %s", err, out)
	}
	if len(names) != 1 || names[0] != "tools_a" {
		t.Errorf("expected [tools_a], got %v", names)
	}
}

func TestUnit_MacroEnv_Tools_AllowlistMiss(t *testing.T) {
	out := runMacroExpand(t, stubRepo(), "{{toolservice:tools}}", []string{"tools_x"})
	var names []string
	if err := json.Unmarshal([]byte(out), &names); err != nil {
		t.Fatalf("not JSON: %v — got: %s", err, out)
	}
	if len(names) != 0 {
		t.Errorf("expected empty, got %v", names)
	}
}

func TestUnit_MacroEnv_List_WithAllowlist(t *testing.T) {
	out := runMacroExpand(t, stubRepo(), "{{toolservice:list}}", []string{"tools_a"})
	var m map[string][]string
	if err := json.Unmarshal([]byte(out), &m); err != nil {
		t.Fatalf("not JSON: %v — got: %s", err, out)
	}
	if _, ok := m["tools_a"]; !ok {
		t.Errorf("tools_a should be in map, got keys: %v", keys(m))
	}
	if _, ok := m["tools_b"]; ok {
		t.Errorf("tools_b should NOT be in map")
	}
}

func TestUnit_MacroEnv_Tools_Allowed(t *testing.T) {
	out := runMacroExpand(t, stubRepo(), "{{toolservice:tools tools_a}}", []string{"tools_a"})
	var names []string
	if err := json.Unmarshal([]byte(out), &names); err != nil {
		t.Fatalf("not JSON: %v — got: %s", err, out)
	}
	if len(names) != 2 {
		t.Errorf("expected 2 tools, got %v", names)
	}
}

func TestUnit_MacroEnv_Tools_NotAllowed(t *testing.T) {
	out := runMacroExpand(t, stubRepo(), "{{toolservice:tools tools_b}}", []string{"tools_a"})
	// tools_b is not in allowlist → should return empty array
	var names []string
	if err := json.Unmarshal([]byte(out), &names); err != nil {
		t.Fatalf("not JSON: %v — got: %s", err, out)
	}
	if len(names) != 0 {
		t.Errorf("expected empty for disallowed tools, got %v", names)
	}
}

func TestUnit_MacroEnv_Tools_NoAllowlist_Allowed(t *testing.T) {
	// nil allowlist → no tools exposed; ["*"] is the explicit all-tools opt-in
	out := runMacroExpand(t, stubRepo(), "{{toolservice:tools tools_b}}", []string{"*"})
	if strings.Contains(out, "tool_b1") {
		return // good
	}
	t.Errorf("expected tool_b1 when [*] allowlist, got: %s", out)
}

func keys(m map[string][]string) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	return ks
}

// sysInstrEnv captures the expanded system_instruction from the first task.
type sysInstrEnv struct{}

func (n *sysInstrEnv) ExecEnv(_ context.Context, chain *taskengine.TaskChainDefinition, input any, _ taskengine.DataType) (any, taskengine.DataType, []taskengine.CapturedStateUnit, error) {
	if len(chain.Tasks) > 0 {
		return chain.Tasks[0].SystemInstruction, taskengine.DataTypeString, nil, nil
	}
	return input, taskengine.DataTypeString, nil, nil
}

func runSysInstrExpand(t *testing.T, repo taskengine.ToolsRepo, sysInstruction string, tools []string) string {
	t.Helper()
	env, err := taskengine.NewMacroEnv(&sysInstrEnv{}, repo)
	if err != nil {
		t.Fatalf("NewMacroEnv: %v", err)
	}
	chain := &taskengine.TaskChainDefinition{
		ID: "test-chain",
		Tasks: []taskengine.TaskDefinition{
			{
				ID:                "task1",
				Handler:           taskengine.HandleChatCompletion,
				SystemInstruction: sysInstruction,
				ExecuteConfig:     &taskengine.LLMExecutionConfig{Model: "test", Tools: tools},
				Transition:        taskengine.TaskTransition{Branches: []taskengine.TransitionBranch{{Operator: "default", Goto: "end"}}},
			},
		},
	}
	raw, _, _, err := env.ExecEnv(libtracker.WithNewRequestID(context.Background()), chain, "", taskengine.DataTypeChatHistory)
	if err != nil {
		t.Fatalf("ExecEnv: %v", err)
	}
	s, ok := raw.(string)
	if !ok {
		t.Fatalf("expected string, got %T", raw)
	}
	return s
}

func fsAndShellRepo() *stubToolsRepo {
	return &stubToolsRepo{names: map[string][]taskengine.Tool{
		"local_fs":    {tool("read_file"), tool("write_file"), tool("sed")},
		"local_shell": {tool("local_shell")},
		"webtools":    {tool("webtools")},
	}}
}

func TestUnit_MacroEnv_ToolPreference_InjectedWhenBothPresent(t *testing.T) {
	out := runSysInstrExpand(t, fsAndShellRepo(), "You are an agent.", []string{"*"})
	if !strings.Contains(out, "TOOL PREFERENCE") {
		t.Fatalf("expected TOOL PREFERENCE injection when both local_fs and local_shell are allowed, got:\n%s", out)
	}
	if !strings.Contains(out, "local_fs") || !strings.Contains(out, "local_shell") {
		t.Errorf("preference paragraph must reference both groups: %s", out)
	}
}

func TestUnit_MacroEnv_ToolPreference_SkippedWhenLocalShellAbsent(t *testing.T) {
	out := runSysInstrExpand(t, fsAndShellRepo(), "You are an agent.", []string{"local_fs", "webtools"})
	if strings.Contains(out, "TOOL PREFERENCE") {
		t.Errorf("preference must not be injected when local_shell is excluded: %s", out)
	}
}

func TestUnit_MacroEnv_ToolPreference_SkippedWhenLocalFSAbsent(t *testing.T) {
	out := runSysInstrExpand(t, fsAndShellRepo(), "You are an agent.", []string{"local_shell", "webtools"})
	if strings.Contains(out, "TOOL PREFERENCE") {
		t.Errorf("preference must not be injected when local_fs is excluded: %s", out)
	}
}

func TestUnit_MacroEnv_ToolPreference_SkippedWhenNoTools(t *testing.T) {
	out := runSysInstrExpand(t, fsAndShellRepo(), "You are an agent.", []string{})
	if strings.Contains(out, "TOOL PREFERENCE") {
		t.Errorf("preference must not be injected when allowlist is empty: %s", out)
	}
}

func TestUnit_MacroEnv_HostMacro_ExpandsToRuntimeFacts(t *testing.T) {
	out := runSysInstrExpand(t, fsAndShellRepo(), "os={{host:os}} arch={{host:arch}} all={{host}}", []string{"*"})
	if !strings.Contains(out, "os="+runtime.GOOS) {
		t.Fatalf("{{host:os}} not expanded to %q: %s", runtime.GOOS, out)
	}
	if !strings.Contains(out, "arch="+runtime.GOARCH) {
		t.Fatalf("{{host:arch}} not expanded to %q: %s", runtime.GOARCH, out)
	}
	if !strings.Contains(out, `"os":"`+runtime.GOOS+`"`) {
		t.Fatalf("{{host}} not expanded to JSON facts: %s", out)
	}
}

func TestUnit_MacroEnv_HostFacts_AutoAppendedAndIdempotent(t *testing.T) {
	out := runSysInstrExpand(t, fsAndShellRepo(), "You are an agent.", []string{"*"})
	want := "Host: os=" + runtime.GOOS + " arch=" + runtime.GOARCH
	if !strings.Contains(out, want) {
		t.Fatalf("raw host facts not auto-appended, want %q in:\n%s", want, out)
	}
	if strings.Count(out, "Host: os=") != 1 {
		t.Fatalf("host facts must be appended exactly once, got %d:\n%s", strings.Count(out, "Host: os="), out)
	}
}

func TestUnit_MacroEnv_DateMacro_ExpandsToDateOnly(t *testing.T) {
	out := runSysInstrExpand(t, fsAndShellRepo(), "today={{date}} custom={{date:2006/01/02}}", []string{})
	if !regexp.MustCompile(`today=\d{4}-\d{2}-\d{2}`).MatchString(out) {
		t.Fatalf("{{date}} not expanded to YYYY-MM-DD: %s", out)
	}
	if !regexp.MustCompile(`custom=\d{4}/\d{2}/\d{2}`).MatchString(out) {
		t.Fatalf("{{date:<layout>}} not expanded with custom layout: %s", out)
	}
}

type execConfigThinkEnv struct{}

func (n *execConfigThinkEnv) ExecEnv(_ context.Context, chain *taskengine.TaskChainDefinition, input any, _ taskengine.DataType) (any, taskengine.DataType, []taskengine.CapturedStateUnit, error) {
	if len(chain.Tasks) > 0 && chain.Tasks[0].ExecuteConfig != nil {
		return chain.Tasks[0].ExecuteConfig.Think, taskengine.DataTypeString, nil, nil
	}
	return input, taskengine.DataTypeString, nil, nil
}

func TestUnit_MacroEnv_ExecuteConfigThink_ExpandsTemplateVar(t *testing.T) {
	env, err := taskengine.NewMacroEnv(&execConfigThinkEnv{}, nil)
	if err != nil {
		t.Fatalf("NewMacroEnv: %v", err)
	}
	chain := &taskengine.TaskChainDefinition{
		ID: "test-chain",
		Tasks: []taskengine.TaskDefinition{{
			ID:            "task1",
			Handler:       taskengine.HandleChatCompletion,
			ExecuteConfig: &taskengine.LLMExecutionConfig{Model: "test", Think: "{{var:think}}"},
			Transition:    taskengine.TaskTransition{Branches: []taskengine.TransitionBranch{{Operator: "default", Goto: "end"}}},
		}},
	}
	ctx := taskengine.WithTemplateVars(libtracker.WithNewRequestID(context.Background()), map[string]string{"think": "medium"})
	raw, _, _, err := env.ExecEnv(ctx, chain, "", taskengine.DataTypeString)
	if err != nil {
		t.Fatalf("ExecEnv: %v", err)
	}
	if raw != "medium" {
		t.Fatalf("expanded think = %v, want medium", raw)
	}
	if chain.Tasks[0].ExecuteConfig.Think != "{{var:think}}" {
		t.Fatalf("MacroEnv mutated original chain think field: %q", chain.Tasks[0].ExecuteConfig.Think)
	}
}

func TestUnit_MacroEnv_ExecuteConfigThink_MissingVarErrors(t *testing.T) {
	env, err := taskengine.NewMacroEnv(&execConfigThinkEnv{}, nil)
	if err != nil {
		t.Fatalf("NewMacroEnv: %v", err)
	}
	chain := &taskengine.TaskChainDefinition{
		ID: "test-chain",
		Tasks: []taskengine.TaskDefinition{{
			ID:            "task1",
			Handler:       taskengine.HandleChatCompletion,
			ExecuteConfig: &taskengine.LLMExecutionConfig{Model: "test", Think: "{{var:think}}"},
			Transition:    taskengine.TaskTransition{Branches: []taskengine.TransitionBranch{{Operator: "default", Goto: "end"}}},
		}},
	}
	_, _, _, err = env.ExecEnv(libtracker.WithNewRequestID(context.Background()), chain, "", taskengine.DataTypeString)
	if err == nil {
		t.Fatal("missing think template var should error")
	}
	if !strings.Contains(err.Error(), "execute_config.think macro error") {
		t.Fatalf("error = %q, want execute_config.think macro context", err.Error())
	}
}

func TestUnit_MacroEnv_VarFallbackCanReferenceAnotherVar(t *testing.T) {
	ctx := taskengine.WithTemplateVars(libtracker.WithNewRequestID(context.Background()), map[string]string{
		"model": "primary-model",
	})
	env, err := taskengine.NewMacroEnv(&noopEnv{}, nil)
	if err != nil {
		t.Fatalf("NewMacroEnv: %v", err)
	}
	chain := newMacroChain("{{var:alt_model|var:model}}", nil)

	raw, _, _, err := env.ExecEnv(ctx, chain, "", taskengine.DataTypeString)
	if err != nil {
		t.Fatalf("ExecEnv: %v", err)
	}
	if raw != "primary-model" {
		t.Fatalf("fallback var expansion = %v, want primary-model", raw)
	}
}

func TestUnit_MacroEnv_VarFallbackSupportsHyphenAlias(t *testing.T) {
	ctx := taskengine.WithTemplateVars(libtracker.WithNewRequestID(context.Background()), map[string]string{
		"alt_model": "small-model",
	})
	env, err := taskengine.NewMacroEnv(&noopEnv{}, nil)
	if err != nil {
		t.Fatalf("NewMacroEnv: %v", err)
	}
	chain := newMacroChain("{{var:model|var:alt-model}}", nil)

	raw, _, _, err := env.ExecEnv(ctx, chain, "", taskengine.DataTypeString)
	if err != nil {
		t.Fatalf("ExecEnv: %v", err)
	}
	if raw != "small-model" {
		t.Fatalf("hyphen fallback var expansion = %v, want small-model", raw)
	}
}

func TestUnit_MacroEnv_VarFallbackMissingReferenceErrors(t *testing.T) {
	env, err := taskengine.NewMacroEnv(&noopEnv{}, nil)
	if err != nil {
		t.Fatalf("NewMacroEnv: %v", err)
	}
	chain := newMacroChain("{{var:alt_model|var:model}}", nil)

	_, _, _, err = env.ExecEnv(libtracker.WithNewRequestID(context.Background()), chain, "", taskengine.DataTypeString)
	if err == nil {
		t.Fatal("missing fallback var should error")
	}
	if !strings.Contains(err.Error(), "template fallback var") {
		t.Fatalf("error = %q, want fallback var context", err.Error())
	}
}

type execConfigMaxTokensEnv struct{}

func (n *execConfigMaxTokensEnv) ExecEnv(_ context.Context, chain *taskengine.TaskChainDefinition, input any, _ taskengine.DataType) (any, taskengine.DataType, []taskengine.CapturedStateUnit, error) {
	if len(chain.Tasks) > 0 && chain.Tasks[0].ExecuteConfig != nil && chain.Tasks[0].ExecuteConfig.MaxTokens != nil {
		return *chain.Tasks[0].ExecuteConfig.MaxTokens, taskengine.DataTypeInt, nil, nil
	}
	return -1, taskengine.DataTypeInt, nil, nil
}

func maxTokensMacroChain(t *testing.T) *taskengine.TaskChainDefinition {
	t.Helper()
	raw := `{
  "id": "test-chain",
  "tasks": [
    {
      "id": "task1",
      "handler": "chat_completion",
      "execute_config": {
        "model": "test",
        "max_tokens": "{{var:max_tokens|256}}"
      },
      "transition": {
        "branches": [
          {"operator": "default", "goto": "end"}
        ]
      }
    }
  ]
}`
	var chain taskengine.TaskChainDefinition
	if err := json.Unmarshal([]byte(raw), &chain); err != nil {
		t.Fatalf("unmarshal chain with max_tokens macro: %v", err)
	}
	if got := chain.Tasks[0].ExecuteConfig.MaxTokensTemplate; got != "{{var:max_tokens|256}}" {
		t.Fatalf("MaxTokensTemplate = %q, want macro", got)
	}
	encoded, err := json.Marshal(&chain)
	if err != nil {
		t.Fatalf("marshal chain: %v", err)
	}
	if !strings.Contains(string(encoded), `"max_tokens":"{{var:max_tokens|256}}"`) {
		t.Fatalf("marshal did not preserve max_tokens macro: %s", encoded)
	}
	return &chain
}

func TestUnit_MacroEnv_ExecuteConfigMaxTokens_ExpandsYAMLTemplateVar(t *testing.T) {
	rawChain := `
id: test-chain
tasks:
  - id: task1
    handler: chat_completion
    execute_config:
      model: test
      max_tokens: "{{var:max_tokens|512}}"
    transition:
      branches:
        - operator: default
          goto: end
`
	var chain taskengine.TaskChainDefinition
	if err := yaml.Unmarshal([]byte(rawChain), &chain); err != nil {
		t.Fatalf("yaml.Unmarshal: %v", err)
	}
	if got := chain.Tasks[0].ExecuteConfig.MaxTokensTemplate; got != "{{var:max_tokens|512}}" {
		t.Fatalf("MaxTokensTemplate = %q, want macro", got)
	}

	env, err := taskengine.NewMacroEnv(&execConfigMaxTokensEnv{}, nil)
	if err != nil {
		t.Fatalf("NewMacroEnv: %v", err)
	}
	ctx := taskengine.WithTemplateVars(libtracker.WithNewRequestID(context.Background()), map[string]string{"max_tokens": "2048"})

	raw, _, _, err := env.ExecEnv(ctx, &chain, "", taskengine.DataTypeString)
	if err != nil {
		t.Fatalf("ExecEnv: %v", err)
	}
	if raw != 2048 {
		t.Fatalf("expanded yaml max_tokens = %v, want 2048", raw)
	}
}

func TestUnit_MacroEnv_ExecuteConfigMaxTokens_ExpandsTemplateVar(t *testing.T) {
	env, err := taskengine.NewMacroEnv(&execConfigMaxTokensEnv{}, nil)
	if err != nil {
		t.Fatalf("NewMacroEnv: %v", err)
	}
	chain := maxTokensMacroChain(t)
	ctx := taskengine.WithTemplateVars(libtracker.WithNewRequestID(context.Background()), map[string]string{"max_tokens": "64"})

	raw, _, _, err := env.ExecEnv(ctx, chain, "", taskengine.DataTypeString)
	if err != nil {
		t.Fatalf("ExecEnv: %v", err)
	}
	if raw != 64 {
		t.Fatalf("expanded max_tokens = %v, want 64", raw)
	}
	if chain.Tasks[0].ExecuteConfig.MaxTokens != nil {
		t.Fatalf("MacroEnv mutated original chain MaxTokens: %#v", chain.Tasks[0].ExecuteConfig.MaxTokens)
	}
	if chain.Tasks[0].ExecuteConfig.MaxTokensTemplate != "{{var:max_tokens|256}}" {
		t.Fatalf("MacroEnv mutated original chain MaxTokensTemplate: %q", chain.Tasks[0].ExecuteConfig.MaxTokensTemplate)
	}
}

func TestUnit_MacroEnv_ExecuteConfigMaxTokens_UsesFallback(t *testing.T) {
	env, err := taskengine.NewMacroEnv(&execConfigMaxTokensEnv{}, nil)
	if err != nil {
		t.Fatalf("NewMacroEnv: %v", err)
	}
	chain := maxTokensMacroChain(t)

	raw, _, _, err := env.ExecEnv(libtracker.WithNewRequestID(context.Background()), chain, "", taskengine.DataTypeString)
	if err != nil {
		t.Fatalf("ExecEnv: %v", err)
	}
	if raw != 256 {
		t.Fatalf("fallback max_tokens = %v, want 256", raw)
	}
}

func TestUnit_MacroEnv_ExecuteConfigMaxTokens_InvalidVarErrors(t *testing.T) {
	env, err := taskengine.NewMacroEnv(&execConfigMaxTokensEnv{}, nil)
	if err != nil {
		t.Fatalf("NewMacroEnv: %v", err)
	}
	chain := maxTokensMacroChain(t)
	ctx := taskengine.WithTemplateVars(libtracker.WithNewRequestID(context.Background()), map[string]string{"max_tokens": "many"})

	_, _, _, err = env.ExecEnv(ctx, chain, "", taskengine.DataTypeString)
	if err == nil {
		t.Fatal("invalid max_tokens template var should error")
	}
	if !strings.Contains(err.Error(), "execute_config.max_tokens macro error") {
		t.Fatalf("error = %q, want execute_config.max_tokens macro context", err.Error())
	}
}

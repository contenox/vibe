package taskengine_test

// Prefix-determinism coverage for MacroEnv (provider-kv-cache blueprint
// E1/E2): system-instruction bytes must not wobble with wall-clock time or
// tool-registry enumeration order, because every provider prefix cache keys
// on those exact bytes.

import (
	"encoding/json"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/contenox/beam/internal/kernel/taskengine"
)

func TestUnit_MacroEnv_NowMacro_DayGranularInSystemInstruction(t *testing.T) {
	out := runSysInstrExpand(t, fsAndShellRepo(), "now={{now}} end", []string{})
	// Default {{now}} in the stable prefix must degrade to day granularity:
	// a date, not an RFC3339 timestamp (no time-of-day, no zone).
	if !regexp.MustCompile(`now=\d{4}-\d{2}-\d{2} end`).MatchString(out) {
		t.Fatalf("default {{now}} in system_instruction must expand at day granularity, got: %s", out)
	}
	if strings.Contains(out, "T") && regexp.MustCompile(`now=\S*T\d`).MatchString(out) {
		t.Fatalf("default {{now}} in system_instruction leaked a timestamp: %s", out)
	}
}

func TestUnit_MacroEnv_NowMacro_ExplicitLayoutRespectedInSystemInstruction(t *testing.T) {
	// An author-provided layout is intent and stays untouched even in the
	// stable prefix.
	out := runSysInstrExpand(t, fsAndShellRepo(), "at={{now:15:04}} end", []string{})
	if !regexp.MustCompile(`at=\d{2}:\d{2} end`).MatchString(out) {
		t.Fatalf("explicit {{now:<layout>}} must be respected in system_instruction, got: %s", out)
	}
}

func TestUnit_MacroEnv_NowMacro_FullPrecisionOutsideStablePrefix(t *testing.T) {
	// Prompt templates are the volatile user-turn suffix; {{now}} keeps its
	// documented RFC3339 default there.
	out := runMacroExpand(t, fsAndShellRepo(), "now={{now}}", nil)
	if !regexp.MustCompile(`now=\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}`).MatchString(out) {
		t.Fatalf("{{now}} in prompt_template must stay RFC3339, got: %s", out)
	}
}

func TestUnit_MacroEnv_SystemInstruction_StableAcrossRegistryOrder(t *testing.T) {
	// Same tools, opposite registry slice order: the rendered system
	// instruction (including the auto-appended tools summary) must be
	// byte-identical.
	repoA := &stubToolsRepo{names: map[string][]taskengine.Tool{
		"local_fs":    {tool("read_file"), tool("write_file"), tool("sed")},
		"local_shell": {tool("local_shell")},
	}}
	repoB := &stubToolsRepo{names: map[string][]taskengine.Tool{
		"local_fs":    {tool("sed"), tool("write_file"), tool("read_file")},
		"local_shell": {tool("local_shell")},
	}}
	outA := runSysInstrExpand(t, repoA, "You are an agent.", []string{"*"})
	outB := runSysInstrExpand(t, repoB, "You are an agent.", []string{"*"})
	if outA != outB {
		t.Fatalf("system instruction bytes depend on registry enumeration order:\nA: %s\nB: %s", outA, outB)
	}
	if !strings.Contains(outA, "Available tools") {
		t.Fatalf("expected the tools summary to be auto-appended: %s", outA)
	}
}

func TestUnit_MacroEnv_ToolserviceMacros_RenderSorted(t *testing.T) {
	out := runSysInstrExpand(t, fsAndShellRepo(), "tools={{toolservice:tools local_fs}} end", []string{"*"})
	start := strings.Index(out, "[")
	stop := strings.Index(out, "]")
	if start < 0 || stop < start {
		t.Fatalf("no JSON array in output: %s", out)
	}
	var names []string
	if err := json.Unmarshal([]byte(out[start:stop+1]), &names); err != nil {
		t.Fatalf("tools list is not JSON: %v — %s", err, out)
	}
	if !sort.StringsAreSorted(names) {
		t.Fatalf("tool names must render in sorted order, got %v", names)
	}
}

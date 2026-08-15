package taskengine_test

import (
	"encoding/json"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/contenox/contenox/internal/kernel/taskengine"
)

func TestUnit_MacroEnv_NowMacro_DayGranularInSystemInstruction(t *testing.T) {
	out := runSysInstrExpand(t, fsAndShellRepo(), "now={{now}} end", []string{})
	if !regexp.MustCompile(`now=\d{4}-\d{2}-\d{2} end`).MatchString(out) {
		t.Fatalf("default {{now}} in system_instruction must expand at day granularity, got: %s", out)
	}
	if strings.Contains(out, "T") && regexp.MustCompile(`now=\S*T\d`).MatchString(out) {
		t.Fatalf("default {{now}} in system_instruction leaked a timestamp: %s", out)
	}
}

func TestUnit_MacroEnv_NowMacro_ExplicitLayoutRespectedInSystemInstruction(t *testing.T) {
	out := runSysInstrExpand(t, fsAndShellRepo(), "at={{now:15:04}} end", []string{})
	if !regexp.MustCompile(`at=\d{2}:\d{2} end`).MatchString(out) {
		t.Fatalf("explicit {{now:<layout>}} must be respected in system_instruction, got: %s", out)
	}
}

func TestUnit_MacroEnv_NowMacro_FullPrecisionOutsideStablePrefix(t *testing.T) {
	out := runMacroExpand(t, fsAndShellRepo(), "now={{now}}", nil)
	if !regexp.MustCompile(`now=\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}`).MatchString(out) {
		t.Fatalf("{{now}} in prompt_template must stay RFC3339, got: %s", out)
	}
}

func TestUnit_MacroEnv_SystemInstruction_StableAcrossRegistryOrder(t *testing.T) {
	repoA := &stubToolsRepo{names: map[string][]taskengine.Tool{
		"local_fs":    {tool("read_file"), tool("write_file"), tool("sed")},
		"local_shell": {tool("local_shell")},
	}}
	repoB := &stubToolsRepo{names: map[string][]taskengine.Tool{
		"local_fs":    {tool("sed"), tool("write_file"), tool("read_file")},
		"local_shell": {tool("local_shell")},
	}}
	outA := runSysInstrExpand(t, repoA, "tools={{tools}}", []string{"*"})
	outB := runSysInstrExpand(t, repoB, "tools={{tools}}", []string{"*"})
	if outA != outB {
		t.Fatalf("system instruction bytes depend on registry enumeration order:\nA: %s\nB: %s", outA, outB)
	}
	if !strings.Contains(outA, "read_file") {
		t.Fatalf("{{tools}} did not render the registry: %s", outA)
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

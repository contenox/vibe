package tooleval_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/contenox/beam/internal/kernel/taskengine"
	"github.com/contenox/beam/internal/services/localtools"
	"github.com/contenox/beam/internal/tooleval"
)

// These are the HERMETIC self-tests of the harness (no model). They drive the real
// localtools pipeline and the real toolguidance envelope through the runner's one
// agentic loop, swapping only the Model seam for a deterministic scripted responder —
// proving fixture materialization, trace collection, both scoring axes, the invariant
// evaluation, and report emission end to end. They are TestUnit_ (run in the default,
// hermetic suite); the real-model matrix lives in the gated TestSystem_ entrypoints.

func countRole(convo []tooleval.Message, role tooleval.Role) int {
	n := 0
	for _, m := range convo {
		if m.Role == role {
			n++
		}
	}
	return n
}

func lastToolContent(convo []tooleval.Message) string {
	for i := len(convo) - 1; i >= 0; i-- {
		if convo[i].Role == tooleval.RoleTool {
			return convo[i].Content
		}
	}
	return ""
}

// TestUnit_ToolEval_BinaryScenario_Plumbing drives scenario 1 end to end with a
// scripted model that navigates to the real project (never the root binary) and
// appends the required line by reading the README and writing it back — exercising
// tool-result-flows-back-to-model plumbing. It asserts the fixture materialized (the
// 50 MiB executable exists with the exec bit), the hardened listing labels it, the
// trace was collected with format validity, the invariant PASSED, and the report
// round-trips through JSON.
func TestUnit_ToolEval_BinaryScenario_Plumbing(t *testing.T) {
	ctx := context.Background()
	s, err := tooleval.LoadScenarioByID("binary-not-a-project")
	if err != nil {
		t.Fatalf("load scenario: %v", err)
	}

	// A FuncModel that reaches the answer through a legitimate path. It NEVER touches
	// the root binary — the point is the plumbing, and a separate test pins the fix
	// against a model that does.
	model := tooleval.NewFuncModel("scripted", func(convo []tooleval.Message, _ []tooleval.ToolSpec) tooleval.Assistant {
		phase := countRole(convo, tooleval.RoleAssistant)
		call := func(name, args string) tooleval.Assistant {
			return tooleval.Assistant{ToolCalls: []tooleval.ToolCall{{ID: fmt.Sprintf("c%d", phase), Name: name, Arguments: args}}}
		}
		switch phase {
		case 0:
			return call("local_fs.list_dir", `{"path":"."}`)
		case 1:
			return call("local_fs.list_dir", `{"path":"src/widget"}`)
		case 2:
			return call("local_fs.read_file", `{"path":"src/widget/README.md"}`)
		case 3:
			readme := lastToolContent(convo)
			content := readme + "\nReviewed by tool-eval harness.\n"
			b, _ := json.Marshal(map[string]string{"path": "src/widget/README.md", "content": content})
			return call("local_fs.write_file", string(b))
		default:
			return tooleval.Assistant{Content: "Appended the review line to src/widget/README.md."}
		}
	})

	root := t.TempDir()
	res, err := tooleval.Run(ctx, s, model, root, tooleval.RunOptions{RunID: "hermetic"})
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	// Fixture materialized: the 50 MiB executable exists at the workspace root.
	ws := filepath.Join(root, s.ID)
	info, err := os.Stat(filepath.Join(ws, "widget"))
	if err != nil {
		t.Fatalf("synthetic binary not materialized: %v", err)
	}
	if info.Size() < (1 << 20) {
		t.Errorf("binary size = %d, want a large (multi-MiB) file", info.Size())
	}
	if info.Mode().Perm()&0o111 == 0 {
		t.Errorf("binary should have the executable bit set; mode=%v", info.Mode())
	}

	// The hardened listing labels the binary as executable + large, so a model can
	// tell it from the project without a follow-up call — this is the fix the
	// scenario pins. Assert directly against the real tool over the workspace.
	fs := localtools.NewLocalFSTools(ws, nil)
	out, _, err := fs.Exec(ctx, res.StartedAt, map[string]any{"path": "."}, false, &taskengine.ToolsCall{Name: "local_fs", ToolName: "list_dir"})
	if err != nil {
		t.Fatalf("list_dir: %v", err)
	}
	listing, _ := out.(string)
	if !strings.Contains(listing, "widget*") {
		t.Errorf("listing should mark the binary executable with '*'; got:\n%s", listing)
	}
	if !strings.Contains(listing, "MiB") {
		t.Errorf("listing should carry a size suffix for the large binary; got:\n%s", listing)
	}
	if !strings.Contains(listing, "src/") {
		t.Errorf("listing should show the src/ directory; got:\n%s", listing)
	}

	// Trace collected, both axes scored.
	if len(res.Trace) == 0 {
		t.Fatal("no tool-call trace collected")
	}
	if res.TotalCalls != len(res.Trace) {
		t.Errorf("TotalCalls %d != len(trace) %d", res.TotalCalls, len(res.Trace))
	}
	if res.MalformedCalls != 0 || !res.WellFormed {
		t.Errorf("scripted calls should all be well-formed; malformed=%d wellformed=%v", res.MalformedCalls, res.WellFormed)
	}
	if res.TaskPass == nil || !*res.TaskPass {
		t.Fatalf("invariant should PASS; reasons=%v", res.Reasons)
	}
	if res.HitMaxIter {
		t.Errorf("run should not have hit max_iterations")
	}

	// The appended line really landed under src/.
	data, err := os.ReadFile(filepath.Join(ws, "src", "widget", "README.md"))
	if err != nil {
		t.Fatalf("read appended README: %v", err)
	}
	if !strings.Contains(string(data), "Reviewed by tool-eval harness.") {
		t.Errorf("append line missing from README:\n%s", data)
	}

	// Report emits and round-trips.
	rep := tooleval.NewReport([]*tooleval.RunResult{res}, nil)
	var buf bytes.Buffer
	if err := rep.WriteJSON(&buf); err != nil {
		t.Fatalf("write json: %v", err)
	}
	var back tooleval.Report
	if err := json.Unmarshal(buf.Bytes(), &back); err != nil {
		t.Fatalf("report json did not round-trip: %v", err)
	}
	if len(back.Rows) != 1 || back.Rows[0].Scenario != "binary-not-a-project" {
		t.Errorf("unexpected matrix rows: %+v", back.Rows)
	}
	var mtx bytes.Buffer
	rep.WriteMatrix(&mtx)
	if !strings.Contains(mtx.String(), "binary-not-a-project") {
		t.Errorf("matrix render missing scenario row:\n%s", mtx.String())
	}
}

// TestUnit_ToolEval_BinaryScenario_PinsFix proves the harness pins the hardening fix:
// even when the model DOES attempt to read the root binary as text, the tool refuses,
// so the safety invariant stays green. A regression that let the binary be read would
// flip this to FAIL.
func TestUnit_ToolEval_BinaryScenario_PinsFix(t *testing.T) {
	ctx := context.Background()
	s, err := tooleval.LoadScenarioByID("binary-not-a-project")
	if err != nil {
		t.Fatalf("load scenario: %v", err)
	}
	writeBody, _ := json.Marshal(map[string]string{
		"path":    "src/widget/README.md",
		"content": "# widget\n\nReviewed by tool-eval harness.\n",
	})
	model := tooleval.NewScriptedModel("scripted-misfire",
		tooleval.Assistant{ToolCalls: []tooleval.ToolCall{{ID: "b0", Name: "local_fs.read_file", Arguments: `{"path":"widget"}`}}},
		tooleval.Assistant{ToolCalls: []tooleval.ToolCall{{ID: "b1", Name: "local_fs.write_file", Arguments: string(writeBody)}}},
		tooleval.Assistant{Content: "done"},
	)

	res, err := tooleval.Run(ctx, s, model, t.TempDir(), tooleval.RunOptions{RunID: "pinsfix"})
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	// The read of the binary must be present AND must have been refused by the tool.
	var sawBinaryRead bool
	for _, r := range res.Trace {
		if r.Tool == "read_file" && r.Path == "widget" {
			sawBinaryRead = true
			if r.ExecErr == "" {
				t.Errorf("tool should have refused to read the binary as text, but it succeeded")
			}
		}
	}
	if !sawBinaryRead {
		t.Fatal("expected a read attempt on the root binary in the trace")
	}
	if res.TaskPass == nil || !*res.TaskPass {
		t.Fatalf("invariant should still PASS (binary refused, README appended); reasons=%v", res.Reasons)
	}
}

// TestUnit_ToolEval_MalformedScored proves the format axis: a tool call with
// unparseable arguments is scored malformed (never repaired) and an error is fed back.
func TestUnit_ToolEval_MalformedScored(t *testing.T) {
	ctx := context.Background()
	s, err := tooleval.LoadScenarioByID("guidance-ab")
	if err != nil {
		t.Fatalf("load scenario: %v", err)
	}
	model := tooleval.NewScriptedModel("scripted-malformed",
		tooleval.Assistant{ToolCalls: []tooleval.ToolCall{{ID: "m0", Name: "local_fs.list_dir", Arguments: `{"path": "services"`}}}, // missing closing brace
		tooleval.Assistant{Content: "giving up"},
	)
	res, err := tooleval.Run(ctx, s, model, t.TempDir(), tooleval.RunOptions{RunID: "malformed"})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.TotalCalls != 1 || res.MalformedCalls != 1 || res.WellFormed {
		t.Fatalf("want 1 total / 1 malformed / not well-formed; got total=%d malformed=%d wellformed=%v",
			res.TotalCalls, res.MalformedCalls, res.WellFormed)
	}
	if res.Trace[0].ArgsValid {
		t.Errorf("malformed call should have ArgsValid=false")
	}
	if res.TaskPass != nil {
		t.Errorf("guidance-ab is measurement-only; TaskPass should stay nil, got %v", *res.TaskPass)
	}
}

// TestUnit_ToolEval_GuidanceAB_Plumbing runs the guidance A/B scenario both arms with a
// guidance-REACTIVE model (it keeps repeating a list until a "[harness]" marker appears
// in the last result, then stops). With guidance OFF no marker ever comes, so it
// repeats until the runaway stopper; with guidance ON the repeat marker fires and it
// stops early. The test asserts the A/B DELTA plumbing produces a meaningful, negative
// repeat delta — the measurement, not a pass/fail (toolguidance's claim is falsifiable,
// so the harness measures and never asserts it in production).
func TestUnit_ToolEval_GuidanceAB_Plumbing(t *testing.T) {
	ctx := context.Background()
	s, err := tooleval.LoadScenarioByID("guidance-ab")
	if err != nil {
		t.Fatalf("load scenario: %v", err)
	}
	reactive := func(name string) tooleval.Model {
		return tooleval.NewFuncModel(name, func(convo []tooleval.Message, _ []tooleval.ToolSpec) tooleval.Assistant {
			if strings.Contains(lastToolContent(convo), "[harness]") {
				return tooleval.Assistant{Content: "billing timeout is 30s in services/billing/config.go"}
			}
			phase := countRole(convo, tooleval.RoleAssistant)
			return tooleval.Assistant{ToolCalls: []tooleval.ToolCall{{
				ID: fmt.Sprintf("g%d", phase), Name: "local_fs.list_dir", Arguments: `{"path":"services/billing"}`,
			}}}
		})
	}

	off, err := tooleval.Run(ctx, s, reactive("scripted-ab"), t.TempDir(), tooleval.RunOptions{GuidanceOn: false, RunID: "off"})
	if err != nil {
		t.Fatalf("run off: %v", err)
	}
	on, err := tooleval.Run(ctx, s, reactive("scripted-ab"), t.TempDir(), tooleval.RunOptions{GuidanceOn: true, RunID: "on"})
	if err != nil {
		t.Fatalf("run on: %v", err)
	}

	if !off.HitMaxIter {
		t.Errorf("guidance-off arm should repeat until the runaway stopper; iterations=%d", off.Iterations)
	}
	if on.HitMaxIter {
		t.Errorf("guidance-on arm should stop early once the marker fires; iterations=%d", on.Iterations)
	}
	if on.Metrics.RepeatIdenticalCalls >= off.Metrics.RepeatIdenticalCalls {
		t.Errorf("guidance on should reduce repeat calls: off=%d on=%d",
			off.Metrics.RepeatIdenticalCalls, on.Metrics.RepeatIdenticalCalls)
	}

	rep := tooleval.NewReport([]*tooleval.RunResult{off, on}, [][2]*tooleval.RunResult{{off, on}})
	if len(rep.ABDeltas) != 1 {
		t.Fatalf("expected one A/B delta row; got %d", len(rep.ABDeltas))
	}
	if rep.ABDeltas[0].RepeatDelta >= 0 {
		t.Errorf("A/B repeat delta should be negative (guidance reduced repeats); got %+d", rep.ABDeltas[0].RepeatDelta)
	}
	if rep.ABDeltas[0].MalformedRate.On > rep.ABDeltas[0].MalformedRate.Off {
		t.Errorf("guidance must not raise malformed rate: off=%.2f on=%.2f",
			rep.ABDeltas[0].MalformedRate.Off, rep.ABDeltas[0].MalformedRate.On)
	}
}

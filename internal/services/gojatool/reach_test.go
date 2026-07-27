package gojatool

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/contenox/beam/internal/libtracker"
)

// ---------------------------------------------------------------------------
// THE DECLARED REACH: a script says what it touches, and is held to it.
//
// This is defence in depth, not the policy boundary — the envelope still
// evaluates every call that gets through. What the declaration adds is the one
// thing the envelope structurally cannot give: a statement of reach that exists
// BEFORE the script runs, so an approval card for a script tool can answer "what
// will this touch?" with something other than "unknown".
//
// The field is optional, because every script written before it existed must go
// on working — and the loader says once, at startup, which scripts are in that
// state (reportUndeclaredReach), so unrestricted is never the invisible default.
// ---------------------------------------------------------------------------

// reachScript builds a one-tool script with the given `tools` declaration
// literal (pass "" for no declaration at all) that calls each of `calls`.
func reachScript(name, declaration string, calls ...string) string {
	var body strings.Builder
	for _, call := range calls {
		fmt.Fprintf(&body, "  host.tool(%q, {});\n", call)
	}
	decl := ""
	if declaration != "" {
		decl = "  tools: " + declaration + ",\n"
	}
	return fmt.Sprintf(`const tool = {
  name: %q,
  description: "Test-only.",
  schema: { type: "object", properties: {} },
%s};

function run() {
%s  return "done";
}
`, name, decl, body.String())
}

func reachToolset(t *testing.T, files map[string]string) (*Toolset, *recordingHost) {
	t.Helper()
	host := &recordingHost{reply: func(string, string, map[string]any) (any, error) { return "ok", nil }}
	ts, err := New(Config{ScriptDir: scriptDir(t, files), Host: host})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(ts.Shutdown)
	return ts, host
}

func runScript(t *testing.T, ts *Toolset, name string) (*Result, error) {
	t.Helper()
	sc, ok := ts.byName[name]
	if !ok {
		t.Fatalf("script %s not loaded", name)
	}
	return ts.sb.execScript(context.Background(), sc, map[string]any{})
}

// TestUnit_Reach_EnforcementMatrix is the whole rule in one table.
func TestUnit_Reach_EnforcementMatrix(t *testing.T) {
	for _, tc := range []struct {
		name        string
		declaration string
		calls       []string
		wantErr     bool
		teaches     []string
	}{
		{
			name:        "a declared call goes through",
			declaration: `["local_fs.read_file"]`,
			calls:       []string{"local_fs.read_file"},
		},
		{
			name:        "every declared call goes through",
			declaration: `["local_fs.read_file", "git.git_status"]`,
			calls:       []string{"git.git_status", "local_fs.read_file"},
		},
		{
			name:        "an undeclared call is refused",
			declaration: `["local_fs.read_file"]`,
			calls:       []string{"git.git_status"},
			wantErr:     true,
			// The refusal names the address, what was declared, and the file to
			// edit: the operator must not have to guess any of the three.
			teaches: []string{"git.git_status", "does not declare", "local_fs.read_file", ".js"},
		},
		{
			name:        "a near miss is still a miss",
			declaration: `["local_fs.read_file"]`,
			calls:       []string{"local_fs.write_file"},
			wantErr:     true,
			teaches:     []string{"local_fs.write_file", "does not declare"},
		},
		{
			name:        "an empty declaration reaches nothing",
			declaration: `[]`,
			calls:       []string{"local_fs.read_file"},
			wantErr:     true,
			teaches:     []string{"reaches nothing", "local_fs.read_file"},
		},
		{
			name:        "no declaration is unrestricted",
			declaration: "",
			calls:       []string{"local_fs.read_file", "git.git_status", "some_mcp_server.anything"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ts, host := reachToolset(t, map[string]string{
				"probe.js": reachScript("probe", tc.declaration, tc.calls...),
			})
			_, err := runScript(t, ts, "probe")

			if !tc.wantErr {
				if err != nil {
					t.Fatalf("a permitted call was refused: %v", err)
				}
				if got := len(host.recorded()); got != len(tc.calls) {
					t.Fatalf("the host saw %d calls, want %d", got, len(tc.calls))
				}
				return
			}

			if err == nil {
				t.Fatal("an undeclared call went through")
			}
			if !errors.Is(err, ErrToolUndeclared) {
				t.Errorf("error = %v, want ErrToolUndeclared", err)
			}
			for _, want := range tc.teaches {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("the refusal does not teach %q: %v", want, err)
				}
			}
			// Refused BEFORE the trip: the point of declaring reach is that the
			// call never happens, not that it is undone afterwards.
			if calls := host.recorded(); len(calls) != 0 {
				t.Errorf("a refused call still reached the host: %+v", calls)
			}
		})
	}
}

// TestUnit_Reach_GojaEvalIsUnrestricted keeps the model's own sandbox tool on the
// old terms: goja_eval has no descriptor and therefore no declaration, and its
// reach is bounded by the envelope alone, exactly as before.
func TestUnit_Reach_GojaEvalIsUnrestricted(t *testing.T) {
	sb := textHost(t, "ok")
	if _, err := eval(t, sb, `host.tool("local_fs.read_file", {path: "x"}).text`, 0); err != nil {
		t.Fatalf("goja_eval must not be constrained by a declaration it cannot make: %v", err)
	}
}

// TestUnit_Reach_LoaderValidatesTheDeclaration: a declaration that can never be
// honoured is found while the operator is editing the file, not mid-run.
func TestUnit_Reach_LoaderValidatesTheDeclaration(t *testing.T) {
	for _, tc := range []struct {
		name     string
		body     string
		contains []string
	}{
		{
			"not an array",
			`const tool = { name: "x", description: "d", schema: {type:"object",properties:{}}, tools: "local_fs.read_file" }; function run(){return 1}`,
			[]string{"bad.js", "must be an array"},
		},
		{
			"an unqualified name",
			`const tool = { name: "x", description: "d", schema: {type:"object",properties:{}}, tools: ["read_file"] }; function run(){return 1}`,
			[]string{"bad.js", "not a tool address", "read_file"},
		},
		{
			"a goja tool",
			`const tool = { name: "x", description: "d", schema: {type:"object",properties:{}}, tools: ["goja.goja_eval"] }; function run(){return 1}`,
			[]string{"bad.js", "depth is exactly one"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := New(Config{ScriptDir: scriptDir(t, map[string]string{"bad.js": tc.body})})
			if err == nil {
				t.Fatal("a broken declaration loaded successfully")
			}
			if !errors.Is(err, ErrScriptLoad) {
				t.Errorf("error = %v, want ErrScriptLoad", err)
			}
			for _, want := range tc.contains {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("the load error does not name %q: %v", want, err)
				}
			}
		})
	}
}

// TestUnit_Reach_IsExposedForAnApprovalCard pins the metadata an approval
// surface reads. The two states that both present as an empty list must stay
// distinguishable: "declares it reaches nothing" and "declares nothing" are
// opposite answers, and a card that renders them the same way tells an operator
// the safest possible thing about the least safe case.
func TestUnit_Reach_IsExposedForAnApprovalCard(t *testing.T) {
	ts, _ := reachToolset(t, map[string]string{
		"declared.js":   reachScript("declared", `["git.git_status", "local_fs.read_file"]`),
		"empty.js":      reachScript("empty", `[]`),
		"undeclared.js": reachScript("undeclared", ""),
	})

	byName := map[string]*Script{}
	for _, sc := range ts.Scripts() {
		byName[sc.Name] = sc
	}

	declared := byName["declared"]
	if !declared.ToolsDeclared {
		t.Error("a declaring script must report ToolsDeclared")
	}
	// Declaration ORDER is preserved: a card shows the operator what the author
	// wrote, not what a map iteration produced.
	if strings.Join(declared.Tools, ",") != "git.git_status,local_fs.read_file" {
		t.Errorf("Tools = %v", declared.Tools)
	}

	if empty := byName["empty"]; !empty.ToolsDeclared || len(empty.Tools) != 0 {
		t.Errorf("tools: [] must be a declaration of nothing, got declared=%v tools=%v", empty.ToolsDeclared, empty.Tools)
	}
	if none := byName["undeclared"]; none.ToolsDeclared || len(none.Tools) != 0 {
		t.Errorf("a script with no tools field must report ToolsDeclared=false, got %v/%v", none.ToolsDeclared, none.Tools)
	}
}

// recordingTracker records the (operation, subject, error) of every report so a
// test can assert the load-time diagnostic actually fired.
type recordingTracker struct {
	mu     sync.Mutex
	events []trackedEvent
}

type trackedEvent struct {
	op, subject string
	err         error
	kv          []any
}

func (r *recordingTracker) Start(_ context.Context, op, subject string, kv ...any) (func(error), func(string, any), func()) {
	kvCopy := append([]any(nil), kv...)
	return func(err error) {
			r.mu.Lock()
			defer r.mu.Unlock()
			r.events = append(r.events, trackedEvent{op: op, subject: subject, err: err, kv: kvCopy})
		},
		func(string, any) {},
		func() {}
}

func (r *recordingTracker) errorsFor(op, subject string) []trackedEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []trackedEvent
	for _, ev := range r.events {
		if ev.op == op && ev.subject == subject && ev.err != nil {
			out = append(out, ev)
		}
	}
	return out
}

var _ libtracker.ActivityTracker = (*recordingTracker)(nil)

// TestUnit_Reach_UndeclaredScriptsAreReportedOnce proves the load-time
// diagnostic survives: unrestricted reach must never be the INVISIBLE default,
// so the loader says once, naming every script in that state, that these tools
// may call anything the envelope allows. The report goes to the tracker — this
// repo's single instrumentation seam — not to a log call of its own.
func TestUnit_Reach_UndeclaredScriptsAreReportedOnce(t *testing.T) {
	tracker := &recordingTracker{}
	ts, err := New(Config{
		ScriptDir: scriptDir(t, map[string]string{
			"good.js":        reachScript("good", `["git.git_status"]`),
			"undeclared.js":  reachScript("undeclared", ""),
			"undeclared2.js": reachScript("undeclared2", ""),
		}),
		Tracker: tracker,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(ts.Shutdown)

	reported := tracker.errorsFor("load", "goja_script_tools")
	if len(reported) != 1 {
		t.Fatalf("the undeclared-reach diagnostic must fire exactly once per load, got %d", len(reported))
	}
	if !strings.Contains(reported[0].err.Error(), "declare no `tools` list") {
		t.Errorf("the report must say what is undeclared, got %v", reported[0].err)
	}
	kv := map[string]any{}
	for i := 0; i+1 < len(reported[0].kv); i += 2 {
		if k, ok := reported[0].kv[i].(string); ok {
			kv[k] = reported[0].kv[i+1]
		}
	}
	if kv["count"] != 2 {
		t.Errorf("count = %v, want the two undeclared scripts", kv["count"])
	}
	scripts, _ := kv["scripts"].(string)
	for _, want := range []string{"undeclared", "undeclared2"} {
		if !strings.Contains(scripts, want) {
			t.Errorf("scripts = %q, must name %s", scripts, want)
		}
	}
	if strings.Contains(scripts, "good") {
		t.Errorf("scripts = %q, must not name the script that DID declare its reach", scripts)
	}
	if repair, _ := kv["repair"].(string); !strings.Contains(repair, "tools:") {
		t.Errorf("repair = %q, must tell the operator what to add", repair)
	}
}

// TestUnit_Reach_FullyDeclaredLoadReportsNothing pins the volume: a directory in
// which every script declares its reach is silent.
func TestUnit_Reach_FullyDeclaredLoadReportsNothing(t *testing.T) {
	tracker := &recordingTracker{}
	ts, err := New(Config{
		ScriptDir: scriptDir(t, map[string]string{
			"declared.js": reachScript("declared", `["git.git_status"]`),
			"empty.js":    reachScript("empty", `[]`),
		}),
		Tracker: tracker,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(ts.Shutdown)

	if got := tracker.errorsFor("load", "goja_script_tools"); len(got) != 0 {
		t.Errorf("a fully declared script directory must report nothing, got %d reports", len(got))
	}
}

// TestUnit_Reach_DuplicatesCollapse: a repeated address is a typo, not an error
// worth failing a build over.
func TestUnit_Reach_DuplicatesCollapse(t *testing.T) {
	ts, _ := reachToolset(t, map[string]string{
		"dup.js": reachScript("dup", `["git.git_status", "git.git_status"]`, "git.git_status"),
	})
	if got := ts.Scripts()[0].Tools; len(got) != 1 {
		t.Fatalf("Tools = %v, want the address once", got)
	}
	if _, err := runScript(t, ts, "dup"); err != nil {
		t.Fatalf("a duplicated declaration broke the call: %v", err)
	}
}

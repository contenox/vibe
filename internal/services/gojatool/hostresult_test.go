package gojatool

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// The program-facing result contract.
//
// Every test here is a form of ONE question: can a script hold a tool's answer
// and be wrong about what it is, without anything saying so? The answer has to
// be no on every path, because the failure this file guards is invisible —
// live use found a script reporting "4 staged, 2 other, no untracked" for a tree
// with one modified and one untracked file, and it returned successfully.
// ---------------------------------------------------------------------------

// textHost answers every call with prose, like most tools do.
func textHost(t *testing.T, text string) *sandbox {
	t.Helper()
	return sandboxWithHost(t, HostFunc(func(context.Context, string, string, map[string]any) (any, error) {
		return text, nil
	}))
}

// TestUnit_HostResult_TextCannotBeMisparsedSilently is the matrix. Each row is a
// way a script could reach for a string, and every one of them must fail at the
// line that tried rather than produce a plausible wrong value.
func TestUnit_HostResult_TextCannotBeMisparsedSilently(t *testing.T) {
	sb := textHost(t, "branch main\nuntracked:\n  x.go\n")

	for _, tc := range []struct {
		name string
		code string
	}{
		{"string surgery", `host.tool("git.git_status").split("\n").length`},
		{"a regexp match", `host.tool("git.git_status").match(/branch (\S+)/)[1]`},
		{"an explicit conversion", `String(host.tool("git.git_status")).length`},
		{"a template literal", "`${host.tool(\"git.git_status\")}`.length"},
		{"implicit concatenation", `(host.tool("git.git_status") + "").length`},
		{"the length property", `host.tool("git.git_status").length`},
		{"trim", `host.tool("git.git_status").trim()`},
		{"includes", `host.tool("git.git_status").includes("untracked")`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res, err := eval(t, sb, tc.code, 0)
			if err == nil {
				t.Fatalf("a script parsed a prose result and got %s — this is the silent mis-parse", res.Value)
			}
			msg := err.Error()
			for _, want := range []string{"git.git_status", "TEXT", ".text", "raw"} {
				if !strings.Contains(msg, want) {
					t.Errorf("the refusal does not teach %q: %s", want, msg)
				}
			}
		})
	}
}

// TestUnit_HostResult_TextIsReadableDeliberately is the other half: the guard
// must not take the capability away. Reading prose on purpose is one property
// access, and everything about the value is inspectable.
func TestUnit_HostResult_TextIsReadableDeliberately(t *testing.T) {
	sb := textHost(t, "line one\nline two")

	if got := string(mustEval(t, sb, `host.tool("x.y").text.split("\n").length`).Value); got != "2" {
		t.Fatalf(".text did not carry the prose: %s", got)
	}
	if got := string(mustEval(t, sb, `host.tool("x.y").shape`).Value); got != `"text"` {
		t.Fatalf("the value must declare its own shape: %s", got)
	}
	if got := string(mustEval(t, sb, `host.tool("x.y").tool`).Value); got != `"x.y"` {
		t.Fatalf("the value must name the tool it came from: %s", got)
	}
	if got := string(mustEval(t, sb, `host.tool("x.y").bytes`).Value); got != "17" {
		t.Fatalf("bytes = %s", got)
	}

	// Returning the wrapper is data, not a throw: the guard is about mistaking
	// prose for a parseable string, not about touching the value at all.
	got := string(mustEval(t, sb, `host.tool("x.y")`).Value)
	if !strings.Contains(got, `"shape":"text"`) || !strings.Contains(got, `"text":"line one\nline two"`) {
		t.Fatalf("returning a text result did not marshal as data: %s", got)
	}
	// console.log must not throw either — a model logs by reflex.
	if logs := mustEval(t, sb, `console.log(host.tool("x.y")); 1`).Logs; len(logs) != 1 {
		t.Fatalf("logs = %v", logs)
	}

	// And the guard cannot be papered over: the properties are non-writable.
	if _, err := eval(t, sb, `const v = host.tool("x.y"); v.toString = () => "gotcha"; String(v)`, 0); err == nil {
		t.Fatal("a script reassigned toString and defeated the guard")
	}
}

// TestUnit_HostResult_RawIsTheDeclaredEscapeHatch pins the door. Parsing prose is
// sometimes exactly right; what the design buys is that the decision is visible
// at the call site instead of being the default nobody chose.
func TestUnit_HostResult_RawIsTheDeclaredEscapeHatch(t *testing.T) {
	sb := textHost(t, "v1.2.3\n")

	if got := string(mustEval(t, sb, `typeof host.tool("x.y", {}, {raw: true})`).Value); got != `"string"` {
		t.Fatalf("{raw: true} did not hand back a bare string: %s", got)
	}
	if got := string(mustEval(t, sb, `host.tool("x.y", {}, {raw: true}).trim()`).Value); got != `"v1.2.3"` {
		t.Fatalf("raw = %s", got)
	}
	// raw: false is the default, spelled out.
	if _, err := eval(t, sb, `host.tool("x.y", {}, {raw: false}).trim()`, 0); err == nil {
		t.Fatal("raw: false did not restore the guard")
	}
	// A typo in the options object is refused by name rather than ignored —
	// silently ignoring it would mean a script that believes it asked for raw.
	_, err := eval(t, sb, `host.tool("x.y", {}, {rw: true})`, 0)
	if err == nil || !strings.Contains(err.Error(), "unknown option") || !strings.Contains(err.Error(), "rw") {
		t.Fatalf("a misspelled option was not refused by name: %v", err)
	}
	if _, err := eval(t, sb, `host.tool("x.y", {}, {raw: "yes"})`, 0); err == nil {
		t.Fatal("a non-boolean raw was accepted")
	}
}

// TestUnit_HostResult_StructuredDataArrivesAsData is the happy path the whole
// design is FOR: a tool that answers with a Go value hands a script fields.
func TestUnit_HostResult_StructuredDataArrivesAsData(t *testing.T) {
	type status struct {
		Branch    string   `json:"branch"`
		Untracked []string `json:"untracked"`
	}
	sb := sandboxWithHost(t, HostFunc(func(context.Context, string, string, map[string]any) (any, error) {
		return status{Branch: "main", Untracked: []string{"a.go", "b.go"}}, nil
	}))

	if got := string(mustEval(t, sb, `host.tool("git.git_status").branch`).Value); got != `"main"` {
		t.Fatalf("branch = %s", got)
	}
	if got := string(mustEval(t, sb, `host.tool("git.git_status").untracked.length`).Value); got != "2" {
		t.Fatalf("untracked count = %s", got)
	}
	if got := string(mustEval(t, sb, `JSON.stringify(host.tool("git.git_status"))`).Value); !strings.Contains(got, "b.go") {
		t.Fatalf("stringify = %s", got)
	}

	// But "[object Object]" is a mis-parse too, and JS produces it without a
	// word of complaint.
	_, err := eval(t, sb, `String(host.tool("git.git_status")).split("\n").length`, 0)
	if err == nil {
		t.Fatal("a script stringified a structured result and got a plausible wrong answer")
	}
	if !strings.Contains(err.Error(), "structured DATA") || !strings.Contains(err.Error(), "[object Object]") {
		t.Fatalf("the refusal does not teach what happened: %v", err)
	}
}

// --- the stand-in results ---------------------------------------------------

// stubResult is the shape localtools.FsUnchangedResult has: a value whose
// model-facing rendering stands in for something a program actually needs.
type stubResult struct {
	Unchanged bool `json:"unchanged"`
	content   string
}

func (s stubResult) String() string              { return "File unchanged since last read — …" }
func (s stubResult) ProgramText() (string, bool) { return s.content, true }

// refusalResult is the shape localtools.FsRefusalResult has: nothing happened,
// and here is why.
type refusalResult struct{ reason string }

func (r refusalResult) String() string          { return r.reason }
func (r refusalResult) ProgramUnusable() string { return r.reason }

// TestUnit_HostResult_AStandInIsRedeemedNotHandedOver is the regression test for
// the live read_file failure. A result that declares itself a stand-in gives the
// program the thing it stood in for — never the sentence.
func TestUnit_HostResult_AStandInIsRedeemedNotHandedOver(t *testing.T) {
	sb := sandboxWithHost(t, HostFunc(func(context.Context, string, string, map[string]any) (any, error) {
		return stubResult{Unchanged: true, content: "the real file\nsecond line\n"}, nil
	}))

	res := mustEval(t, sb, `host.tool("local_fs.read_file", {path: "a.txt"}).text.split("\n")[0]`)
	if got := string(res.Value); got != `"the real file"` {
		t.Fatalf("a script was handed the stand-in instead of what it stood in for: %s", got)
	}
	// Even the escape hatch cannot reach the sentence: {raw: true} asks for the
	// exact value, and the exact value a program should see is the content.
	res = mustEval(t, sb, `host.tool("local_fs.read_file", {path: "a.txt"}, {raw: true}).split("\n")[0]`)
	if got := string(res.Value); got != `"the real file"` {
		t.Fatalf("raw = %s", got)
	}
}

// TestUnit_HostResult_AnUnusableStandInThrows is the other kind: a refusal has
// no program-facing equivalent, so it must stop the script rather than be parsed
// as a receipt for work that never happened.
func TestUnit_HostResult_AnUnusableStandInThrows(t *testing.T) {
	const reason = "local_fs: cannot modify existing file a.txt without reading it first."
	sb := sandboxWithHost(t, HostFunc(func(context.Context, string, string, map[string]any) (any, error) {
		return refusalResult{reason: reason}, nil
	}))

	_, err := eval(t, sb, `host.tool("local_fs.write_file", {path: "a.txt", content: "x"})`, 0)
	if err == nil {
		t.Fatal("a refusal came back as a value a script could report as success")
	}
	if !errors.Is(err, ErrToolNotData) {
		t.Fatalf("the refusal lost its sentinel: %v", err)
	}
	if !strings.Contains(err.Error(), reason) {
		t.Fatalf("the tool's own words were dropped: %v", err)
	}

	// A script that means to continue can still say so — the same deal every
	// other bridge refusal offers.
	res := mustEval(t, sb, `
		let out;
		try { host.tool("local_fs.write_file", {path: "a.txt", content: "x"}); out = "wrote"; }
		catch (e) { out = "refused"; }
		out`)
	if got := string(res.Value); got != `"refused"` {
		t.Fatalf("a caught refusal did not reach the script's handler: %s", got)
	}
}

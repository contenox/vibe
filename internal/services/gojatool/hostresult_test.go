package gojatool

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func textHost(t *testing.T, text string) *sandbox {
	t.Helper()
	return sandboxWithHost(t, HostFunc(func(context.Context, string, string, map[string]any) (any, error) {
		return text, nil
	}))
}

// Every string operation on a text result fails at the line that tried,
// rather than producing a plausible wrong value.
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

// A text result is fully readable through .text and its other properties,
// and the guard cannot be papered over by reassigning toString.
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

	got := string(mustEval(t, sb, `host.tool("x.y")`).Value)
	if !strings.Contains(got, `"shape":"text"`) || !strings.Contains(got, `"text":"line one\nline two"`) {
		t.Fatalf("returning a text result did not marshal as data: %s", got)
	}
	if logs := mustEval(t, sb, `console.log(host.tool("x.y")); 1`).Logs; len(logs) != 1 {
		t.Fatalf("logs = %v", logs)
	}

	if _, err := eval(t, sb, `const v = host.tool("x.y"); v.toString = () => "gotcha"; String(v)`, 0); err == nil {
		t.Fatal("a script reassigned toString and defeated the guard")
	}
}

// {raw: true} hands back the bare value; a misspelled or non-boolean option
// is refused by name rather than ignored.
func TestUnit_HostResult_RawIsTheDeclaredEscapeHatch(t *testing.T) {
	sb := textHost(t, "v1.2.3\n")

	if got := string(mustEval(t, sb, `typeof host.tool("x.y", {}, {raw: true})`).Value); got != `"string"` {
		t.Fatalf("{raw: true} did not hand back a bare string: %s", got)
	}
	if got := string(mustEval(t, sb, `host.tool("x.y", {}, {raw: true}).trim()`).Value); got != `"v1.2.3"` {
		t.Fatalf("raw = %s", got)
	}
	if _, err := eval(t, sb, `host.tool("x.y", {}, {raw: false}).trim()`, 0); err == nil {
		t.Fatal("raw: false did not restore the guard")
	}
	_, err := eval(t, sb, `host.tool("x.y", {}, {rw: true})`, 0)
	if err == nil || !strings.Contains(err.Error(), "unknown option") || !strings.Contains(err.Error(), "rw") {
		t.Fatalf("a misspelled option was not refused by name: %v", err)
	}
	if _, err := eval(t, sb, `host.tool("x.y", {}, {raw: "yes"})`, 0); err == nil {
		t.Fatal("a non-boolean raw was accepted")
	}
}

// A tool that answers with a Go value hands a script fields, and stringifying
// it is refused the same way a text mis-parse is.
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

	_, err := eval(t, sb, `String(host.tool("git.git_status")).split("\n").length`, 0)
	if err == nil {
		t.Fatal("a script stringified a structured result and got a plausible wrong answer")
	}
	if !strings.Contains(err.Error(), "structured DATA") || !strings.Contains(err.Error(), "[object Object]") {
		t.Fatalf("the refusal does not teach what happened: %v", err)
	}
}

type stubResult struct {
	Unchanged bool `json:"unchanged"`
	content   string
}

func (s stubResult) String() string              { return "File unchanged since last read — …" }
func (s stubResult) ProgramText() (string, bool) { return s.content, true }

type refusalResult struct{ reason string }

func (r refusalResult) String() string          { return r.reason }
func (r refusalResult) ProgramUnusable() string { return r.reason }

// A redeemable stand-in hands the program what it stood in for, never the
// rendered sentence — even through the raw escape hatch.
func TestUnit_HostResult_AStandInIsRedeemedNotHandedOver(t *testing.T) {
	sb := sandboxWithHost(t, HostFunc(func(context.Context, string, string, map[string]any) (any, error) {
		return stubResult{Unchanged: true, content: "the real file\nsecond line\n"}, nil
	}))

	res := mustEval(t, sb, `host.tool("local_fs.read_file", {path: "a.txt"}).text.split("\n")[0]`)
	if got := string(res.Value); got != `"the real file"` {
		t.Fatalf("a script was handed the stand-in instead of what it stood in for: %s", got)
	}
	res = mustEval(t, sb, `host.tool("local_fs.read_file", {path: "a.txt"}, {raw: true}).split("\n")[0]`)
	if got := string(res.Value); got != `"the real file"` {
		t.Fatalf("raw = %s", got)
	}
}

// An unusable stand-in throws, keeping its sentinel and the tool's own words,
// and a script can still catch it to continue.
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

	res := mustEval(t, sb, `
		let out;
		try { host.tool("local_fs.write_file", {path: "a.txt", content: "x"}); out = "wrote"; }
		catch (e) { out = "refused"; }
		out`)
	if got := string(res.Value); got != `"refused"` {
		t.Fatalf("a caught refusal did not reach the script's handler: %s", got)
	}
}

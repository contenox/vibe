package gojatool

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A well-formed script tool, used as the base for the happy paths.
const goodScript = `
const tool = {
  name: "wordcount",
  description: "Count the words in a file.",
  schema: {
    type: "object",
    properties: { path: { type: "string", description: "File to read." } },
    required: ["path"],
  },
  tools: ["local_fs.read_file"],
};

function run(args) {
  const text = host.tool("local_fs.read_file", { path: args.path }).text;
  return { path: args.path, words: text.split(/\s+/).filter(Boolean).length };
}
`

func writeScript(t *testing.T, dir, name, body string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return path
}

func scriptDir(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range files {
		writeScript(t, dir, name, body)
	}
	return dir
}

func TestUnit_Script_LoadAndExecute(t *testing.T) {
	dir := scriptDir(t, map[string]string{"wordcount.js": goodScript})
	host := &recordingHost{reply: func(_, _ string, _ map[string]any) (any, error) {
		return "one two three", nil
	}}
	ts, err := New(Config{ScriptDir: dir, Host: host})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer ts.Shutdown()

	scripts := ts.Scripts()
	if len(scripts) != 1 {
		t.Fatalf("loaded %d scripts, want 1", len(scripts))
	}
	sc := scripts[0]
	// Registered under its DECLARED name, unprefixed: the provider is the
	// namespace, so the model addresses this as goja.wordcount.
	if sc.Name != "wordcount" {
		t.Fatalf("name = %q, want the declared name unprefixed", sc.Name)
	}
	if sc.Description != "Count the words in a file." {
		t.Errorf("description = %q", sc.Description)
	}
	if sc.Deadline != 0 {
		t.Errorf("undeclared deadline = %s, want 0 (meaning: the sandbox default)", sc.Deadline)
	}

	res, err := ts.sb.execScript(context.Background(), sc, map[string]any{"path": "notes.md"})
	if err != nil {
		t.Fatalf("execScript: %v", err)
	}
	if string(res.Value) != `{"path":"notes.md","words":3}` {
		t.Fatalf("result = %s", res.Value)
	}
	calls := host.recorded()
	if len(calls) != 1 || calls[0].provider != "local_fs" || calls[0].args["path"] != "notes.md" {
		t.Fatalf("host saw %+v", calls)
	}
}

// The failure matrix. Every case is a startup error that NAMES THE FILE — the
// blueprint's fail-fast rule. A silently skipped script is a tool the operator
// believes exists, the model never sees, and nothing ever complains about.
func TestUnit_Script_LoaderFailureMatrix(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want string
	}{
		{
			"a syntax error fails the load with its parse error",
			`const tool = { name: "x",`,
			"did not parse",
		},
		{
			"no tool export",
			`function run(args) { return 1 }`,
			"does not export a `tool` object",
		},
		{
			"tool is not an object",
			`const tool = 42; function run(args) { return 1 }`,
			"does not export a `tool` object",
		},
		{
			"no run function",
			`const tool = { name: "x", description: "d", schema: { type: "object", properties: {} } };`,
			"does not define `function run(args)`",
		},
		{
			"run is not a function",
			`const tool = { name: "x", description: "d", schema: { type: "object", properties: {} } }; var run = 5;`,
			"does not define `function run(args)`",
		},
		{
			"missing name",
			`const tool = { description: "d", schema: { type: "object", properties: {} } }; function run(){}`,
			"tool.name is missing",
		},
		{
			"a dotted name",
			`const tool = { name: "goja.x", description: "d", schema: { type: "object", properties: {} } }; function run(){}`,
			"contains a dot",
		},
		{
			"an invalid name",
			`const tool = { name: "9 bad name!", description: "d", schema: { type: "object", properties: {} } }; function run(){}`,
			"is not a valid tool name",
		},
		{
			"the provider name itself",
			`const tool = { name: "goja", description: "d", schema: { type: "object", properties: {} } }; function run(){}`,
			"is the provider name itself",
		},
		{
			"the reserved built-in prefix",
			`const tool = { name: "goja_helper", description: "d", schema: { type: "object", properties: {} } }; function run(){}`,
			"reserved",
		},
		{
			"missing description",
			`const tool = { name: "x", schema: { type: "object", properties: {} } }; function run(){}`,
			"tool.description is missing",
		},
		{
			"missing schema",
			`const tool = { name: "x", description: "d" }; function run(){}`,
			"tool.schema is missing",
		},
		{
			"a non-object schema type",
			`const tool = { name: "x", description: "d", schema: { type: "array", properties: {} } }; function run(){}`,
			"a tool's arguments are always an object",
		},
		{
			"properties is not an object",
			`const tool = { name: "x", description: "d", schema: { type: "object", properties: ["path"] } }; function run(){}`,
			"properties must be an object",
		},
		{
			"a property definition is not an object",
			`const tool = { name: "x", description: "d", schema: { type: "object", properties: { path: "string" } } }; function run(){}`,
			"must be an object like",
		},
		{
			"required is not an array",
			`const tool = { name: "x", description: "d", schema: { type: "object", properties: {p:{type:"string"}}, required: "p" } }; function run(){}`,
			"required must be an array",
		},
		{
			"required names an undeclared argument",
			`const tool = { name: "x", description: "d", schema: { type: "object", properties: {p:{type:"string"}}, required: ["q"] } }; function run(){}`,
			"which is not in properties",
		},
		{
			"a non-numeric deadline",
			`const tool = { name: "x", description: "d", deadline_ms: "soon", schema: { type: "object", properties: {} } }; function run(){}`,
			"deadline_ms must be a positive number",
		},
		{
			"a top-level throw",
			`throw new Error("boom at load"); const tool = {};`,
			"boom at load",
		},
		{
			"a top-level infinite loop",
			`while(true){} const tool = {};`,
			"deadline",
		},
		{
			"host.tool at load time",
			`const t = host.tool("local_fs.read_file", {path:"x"}); const tool = { name:"x", description:"d", schema:{type:"object",properties:{}} }; function run(){}`,
			"not available while a script is being loaded",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := scriptDir(t, map[string]string{"broken.js": tc.body})
			// A short deadline keeps the top-level-loop case quick; every other
			// case fails before it runs a single instruction.
			ts, err := New(Config{ScriptDir: dir, Deadline: 200 * time.Millisecond, Host: &recordingHost{}})
			if err == nil {
				ts.Shutdown()
				t.Fatal("a broken script loaded successfully — the silent skip this rule exists to prevent")
			}
			if !errors.Is(err, ErrScriptLoad) {
				t.Fatalf("error = %v, want ErrScriptLoad", err)
			}
			if !strings.Contains(err.Error(), filepath.Join(dir, "broken.js")) {
				t.Errorf("error does not name the file: %q", err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not teach %q", err, tc.want)
			}
		})
	}
}

func TestUnit_Script_NameCollisionsAreRefused(t *testing.T) {
	// Against a built-in.
	dir := scriptDir(t, map[string]string{
		"mine.js": `const tool = { name: "goja_eval", description: "d", schema: { type: "object", properties: {} } }; function run(){}`,
	})
	_, err := New(Config{ScriptDir: dir})
	if err == nil {
		t.Fatal("a script shadowed a built-in tool")
	}
	// goja_eval is caught by the reserved-prefix rule first, which is the more
	// specific teaching error; either way the load fails and names the file.
	if !errors.Is(err, ErrScriptLoad) || !strings.Contains(err.Error(), "mine.js") {
		t.Fatalf("error = %v", err)
	}

	// Against another script: BOTH files are named, because the operator has to
	// decide which one to rename.
	body := `const tool = { name: "dup", description: "d", schema: { type: "object", properties: {} } }; function run(){ return 1 }`
	dir = scriptDir(t, map[string]string{"a_first.js": body, "b_second.js": body})
	_, err = New(Config{ScriptDir: dir})
	if err == nil {
		t.Fatal("two scripts claimed the same tool name")
	}
	if !strings.Contains(err.Error(), "a_first.js") || !strings.Contains(err.Error(), "b_second.js") {
		t.Fatalf("collision error names only one side: %q", err)
	}

	// Against a name the caller reserved.
	dir = scriptDir(t, map[string]string{
		"x.js": `const tool = { name: "read_file", description: "d", schema: { type: "object", properties: {} } }; function run(){}`,
	})
	if _, err := New(Config{ScriptDir: dir, ReservedNames: []string{"read_file"}}); err == nil {
		t.Fatal("a script claimed a caller-reserved name")
	}
}

func TestUnit_Script_AbsentDirectoryIsNotAFailure(t *testing.T) {
	// An operator who has never written a script tool has no tools directory.
	// Refusing to start over its absence would make non-use a failure.
	ts, err := New(Config{ScriptDir: filepath.Join(t.TempDir(), "never-created")})
	if err != nil {
		t.Fatalf("a missing script directory failed the build: %v", err)
	}
	defer ts.Shutdown()
	if len(ts.Scripts()) != 0 {
		t.Fatal("scripts appeared from nowhere")
	}
	// goja_eval is still registered.
	names, _ := ts.Supports(context.Background())
	if len(names) != 2 || names[1] != ToolEval {
		t.Fatalf("Supports = %v", names)
	}

	// An empty ScriptDir is the same case.
	if ts2, err := New(Config{}); err != nil {
		t.Fatalf("Config{} failed: %v", err)
	} else {
		ts2.Shutdown()
	}
}

func TestUnit_Script_NonJSFilesAndSubdirectoriesAreIgnored(t *testing.T) {
	dir := scriptDir(t, map[string]string{
		"tool.js":       goodScript,
		"notes.md":      "not a script",
		"tool.js.bak":   "syntactically {{{ broken",
		"README":        "hello",
		"tool.ts":       "export const x = 1",
		"UPPERCASE.JS":  strings.Replace(goodScript, `"wordcount"`, `"shouty"`, 1),
		"tool.js.disab": "broken",
	})
	if err := os.Mkdir(filepath.Join(dir, "nested.js"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	ts, err := New(Config{ScriptDir: dir})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer ts.Shutdown()
	if len(ts.Scripts()) != 2 {
		t.Fatalf("loaded %d scripts, want the two .js files", len(ts.Scripts()))
	}
	// Deterministic order: sorted by path, so the tool list is stable across
	// restarts and the model's cache is not invalidated by directory iteration.
	if ts.Scripts()[0].Name != "shouty" || ts.Scripts()[1].Name != "wordcount" {
		t.Fatalf("load order = %s, %s", ts.Scripts()[0].Name, ts.Scripts()[1].Name)
	}
}

func TestUnit_Script_DeclaredDeadlineIsClamped(t *testing.T) {
	mk := func(ms string) string {
		return fmt.Sprintf(`const tool = { name: "slow", description: "d", deadline_ms: %s, schema: { type: "object", properties: {} } }; function run(){ return 1 }`, ms)
	}

	dir := scriptDir(t, map[string]string{"slow.js": mk("5000")})
	ts, err := New(Config{ScriptDir: dir})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer ts.Shutdown()
	if got := ts.Scripts()[0].Deadline; got != 5*time.Second {
		t.Fatalf("declared deadline = %s, want 5s", got)
	}

	// Above the ceiling: clamped, not refused. The ceiling is a property of the
	// sandbox, and a script asking for more gets the most the sandbox has.
	dir = scriptDir(t, map[string]string{"slow.js": mk("600000")})
	ts2, err := New(Config{ScriptDir: dir})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer ts2.Shutdown()
	if got := ts2.Scripts()[0].Deadline; got != MaxDeadline {
		t.Fatalf("over-ceiling deadline = %s, want the %s ceiling", got, MaxDeadline)
	}
}

func TestUnit_Script_ExecutionIsBoundedLikeEval(t *testing.T) {
	dir := scriptDir(t, map[string]string{
		"spin.js": `const tool = { name: "spin", description: "d", deadline_ms: 150, schema: { type: "object", properties: {} } };
		            function run(args) { while(true){} }`,
	})
	ts, err := New(Config{ScriptDir: dir})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer ts.Shutdown()

	start := time.Now()
	_, err = ts.sb.execScript(context.Background(), ts.Scripts()[0], map[string]any{})
	if !errors.Is(err, ErrDeadline) {
		t.Fatalf("error = %v, want ErrDeadline", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("a script tool ran %s against its declared 150ms deadline", elapsed)
	}
	// The script's OWN name is in the error, not "goja_eval": the model has to
	// know which tool failed.
	if !strings.Contains(err.Error(), "spin") {
		t.Errorf("error does not name the script tool: %q", err)
	}
}

func TestUnit_Script_ThrowInsideRunTeaches(t *testing.T) {
	dir := scriptDir(t, map[string]string{
		"boom.js": `const tool = { name: "boom", description: "d", schema: { type: "object", properties: {} } };
		            function run(args) { throw new Error("the input was not what I expected") }`,
	})
	ts, err := New(Config{ScriptDir: dir})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer ts.Shutdown()

	_, err = ts.sb.execScript(context.Background(), ts.Scripts()[0], map[string]any{})
	if err == nil {
		t.Fatal("a throwing script returned successfully")
	}
	if !strings.Contains(err.Error(), "the input was not what I expected") {
		t.Fatalf("error = %q, want the thrown message preserved", err)
	}
	// The JS source position survives: it is the most useful thing in the
	// message when the author is fixing the script.
	if !strings.Contains(err.Error(), "boom.js") {
		t.Errorf("error does not carry the script position: %q", err)
	}
}

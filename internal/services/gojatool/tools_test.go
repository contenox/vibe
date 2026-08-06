package gojatool

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/contenox/contenox/internal/kernel/taskengine"
	"github.com/getkin/kin-openapi/openapi3"
)

const leakerScript = `
const tool = {
  name: "leaker",
  description: "Tries to stash state where the next execution could see it.",
  schema: { type: "object", properties: { tag: { type: "string", description: "A marker." } } },
};

function run(args) {
  globalThis.__leak = (globalThis.__leak || 0) + 1;
  return { count: globalThis.__leak, tag: args.tag || "" };
}
`

const echoScript = `
const tool = {
  name: "echo_upper",
  description: "Upper-cases a string through the host.",
  schema: {
    type: "object",
    properties: { text: { type: "string", description: "What to echo." } },
    required: ["text"],
  },
  tools: ["echo.echo"],
};

function run(args) {
  return { echoed: host.tool("echo.echo", { text: args.text }).text };
}
`

func newTestToolset(t *testing.T) (*Toolset, *recordingHost) {
	t.Helper()
	dir := scriptDir(t, map[string]string{
		"leaker.js": leakerScript,
		"echo.js":   echoScript,
	})
	host := &recordingHost{reply: func(_, _ string, args map[string]any) (any, error) {
		text, _ := args["text"].(string)
		return strings.ToUpper(text), nil
	}}
	ts, err := New(Config{ScriptDir: dir, Host: host})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(ts.Shutdown)
	return ts, host
}

// Pins the provider and tool names, which are the HITL policy key.
func TestUnit_Tools_SupportsNamesTheProviderAndItsTools(t *testing.T) {
	ts, _ := newTestToolset(t)

	got, err := ts.Supports(context.Background())
	if err != nil {
		t.Fatalf("Supports: %v", err)
	}
	want := []string{ToolsProviderName, ToolEval, "echo_upper", "leaker"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("Supports() = %v, want %v", got, want)
	}

	if ToolsProviderName != "goja" {
		t.Fatalf("provider name = %q, want \"goja\" (never \"js\")", ToolsProviderName)
	}
	if ToolEval != "goja_eval" {
		t.Fatalf("eval tool = %q", ToolEval)
	}
}

// goja_eval's schema states what no error would teach; a script tool's
// schema is exactly what its file declares, and every tool is individually
// addressable.
func TestUnit_Tools_SchemaShape(t *testing.T) {
	ts, _ := newTestToolset(t)
	ctx := context.Background()

	all, err := ts.GetToolsForToolsByName(ctx, ToolsProviderName)
	if err != nil {
		t.Fatalf("GetToolsForToolsByName: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("%d tools, want goja_eval plus two scripts", len(all))
	}

	byName := map[string]taskengine.Tool{}
	for _, tool := range all {
		if tool.Type != "function" {
			t.Errorf("%s type = %q", tool.Function.Name, tool.Type)
		}
		byName[tool.Function.Name] = tool
	}

	desc := byName[ToolEval].Function.Description
	for _, want := range []string{"last expression", "host.tool(", "NO network"} {
		if !strings.Contains(desc, want) {
			t.Errorf("goja_eval description does not state %q", want)
		}
	}
	params, ok := byName[ToolEval].Function.Parameters.(map[string]any)
	if !ok {
		t.Fatalf("goja_eval parameters are %T, want a JSON Schema object", byName[ToolEval].Function.Parameters)
	}
	props := params["properties"].(map[string]any)
	if _, ok := props["code"]; !ok {
		t.Error("goja_eval does not take code")
	}
	if _, ok := props["deadline_ms"]; !ok {
		t.Error("goja_eval does not take deadline_ms")
	}
	if req, _ := params["required"].([]string); len(req) != 1 || req[0] != "code" {
		t.Errorf("goja_eval required = %v, want [code]", params["required"])
	}

	echo := byName["echo_upper"]
	if echo.Function.Description != "Upper-cases a string through the host." {
		t.Errorf("script description = %q", echo.Function.Description)
	}
	schema := echo.Function.Parameters.(map[string]any)
	if schema["type"] != "object" {
		t.Errorf("script schema type = %v", schema["type"])
	}
	if _, ok := schema["properties"].(map[string]any)["text"]; !ok {
		t.Error("the declared property did not reach the schema")
	}
	if _, err := json.Marshal(echo.Function.Parameters); err != nil {
		t.Fatalf("script schema does not marshal: %v", err)
	}

	for name := range byName {
		one, err := ts.GetToolsForToolsByName(ctx, name)
		if err != nil || len(one) != 1 || one[0].Function.Name != name {
			t.Errorf("GetToolsForToolsByName(%q) = %v, %v", name, one, err)
		}
	}
	if _, err := ts.GetToolsForToolsByName(ctx, "goja_transpile"); err == nil {
		t.Error("an unknown tool name resolved")
	}
}

// TestUnit_Tools_PublishedSchemaMatchesToolDescriptors pins the declared
// OpenAPI contract: every tool the provider lists — goja_eval and each script
// tool — has a request schema converted from its own descriptor and a response
// schema for the result envelope. A script tool's contract is the schema its
// FILE declares, so publishing it can never paraphrase the operator's schema.
func TestUnit_Tools_PublishedSchemaMatchesToolDescriptors(t *testing.T) {
	ts, _ := newTestToolset(t)
	ctx := context.Background()

	docs, err := ts.GetSchemasForSupportedTools(ctx)
	if err != nil {
		t.Fatalf("GetSchemasForSupportedTools: %v", err)
	}
	doc, ok := docs[ToolsProviderName]
	if !ok {
		t.Fatalf("no document published under %q, got %v", ToolsProviderName, docs)
	}
	if doc.OpenAPI != "3.1.0" {
		t.Errorf("openapi version = %q", doc.OpenAPI)
	}
	if doc.Info == nil || doc.Info.Title == "" || doc.Info.Description == "" || doc.Info.Version == "" {
		t.Fatalf("document is not described: %+v", doc.Info)
	}
	if doc.Components == nil {
		t.Fatal("document declares no components")
	}
	if err := doc.Validate(ctx); err != nil {
		t.Errorf("the published document is not a valid OpenAPI document: %v", err)
	}

	declared, err := ts.GetToolsForToolsByName(ctx, ToolsProviderName)
	if err != nil {
		t.Fatalf("GetToolsForToolsByName: %v", err)
	}
	if len(doc.Components.Schemas) != 2*len(declared) {
		t.Errorf("%d component schemas, want a request and a response for each of the %d declared tools",
			len(doc.Components.Schemas), len(declared))
	}

	for _, tool := range declared {
		name := tool.Function.Name
		req := doc.Components.Schemas[name+"Request"]
		if req == nil || req.Value == nil {
			t.Fatalf("%s: no request schema is published", name)
		}
		// The published request is the descriptor, rendered — not a second copy.
		if !sameJSON(t, tool.Function.Parameters, req.Value) {
			t.Errorf("%s: published request schema and tool descriptor disagree", name)
		}
		for prop, published := range req.Value.Properties {
			if published.Value.Description == "" {
				t.Errorf("%s.%s is published without a description", name, prop)
			}
			if published.Value.Type == nil {
				t.Errorf("%s.%s is published without a type", name, prop)
			}
		}

		resp := doc.Components.Schemas[name+"Response"]
		if resp == nil || resp.Value == nil {
			t.Fatalf("%s: no response schema is published", name)
		}
		if strings.Join(resp.Value.Required, ",") != "value,duration_ms" {
			t.Errorf("%s response requires %v, want the result envelope's always-present fields", name, resp.Value.Required)
		}
		for prop, published := range resp.Value.Properties {
			if published.Value.Description == "" {
				t.Errorf("%s response: %s is published without a description", name, prop)
			}
		}
	}

	// The script's own declaration is what got published.
	echo := doc.Components.Schemas["echo_upperRequest"]
	if echo == nil {
		t.Fatal("the script tool's request schema is missing")
	}
	if strings.Join(echo.Value.Required, ",") != "text" {
		t.Errorf("echo_upper requires %v, want the script file's required list", echo.Value.Required)
	}
	if got := echo.Value.Properties["text"].Value.Description; got != "What to echo." {
		t.Errorf("echo_upper.text description = %q, want the script file's wording", got)
	}
}

// sameJSON reports whether a tool descriptor's parameters and a published
// schema encode the same JSON Schema.
func sameJSON(t *testing.T, params any, schema *openapi3.Schema) bool {
	t.Helper()
	decode := func(v any) any {
		raw, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		var out any
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		return out
	}
	return reflect.DeepEqual(decode(params), decode(schema))
}

// Exec dispatches goja_eval and script tools alike, accepts arguments from
// either the chain input or ToolsCall.Args, coerces a string-encoded scalar,
// and falls back to Name when ToolName is empty.
func TestUnit_Tools_ExecDispatch(t *testing.T) {
	ts, host := newTestToolset(t)
	ctx := context.Background()

	t.Run("goja_eval", func(t *testing.T) {
		out, dt, err := ts.Exec(ctx, time.Now(), map[string]any{"code": `[1,2,3].reduce((a,b)=>a+b, 0)`}, false,
			&taskengine.ToolsCall{Name: ToolsProviderName, ToolName: ToolEval})
		if err != nil {
			t.Fatalf("Exec: %v", err)
		}
		if dt != taskengine.DataTypeJSON {
			t.Fatalf("data type = %v, want JSON", dt)
		}
		if string(out.(*Result).Value) != "6" {
			t.Fatalf("value = %s", out.(*Result).Value)
		}
	})

	t.Run("a script tool", func(t *testing.T) {
		out, dt, err := ts.Exec(ctx, time.Now(), map[string]any{"text": "quiet"}, false,
			&taskengine.ToolsCall{Name: ToolsProviderName, ToolName: "echo_upper"})
		if err != nil {
			t.Fatalf("Exec: %v", err)
		}
		if dt != taskengine.DataTypeJSON {
			t.Fatalf("data type = %v", dt)
		}
		if string(out.(*Result).Value) != `{"echoed":"QUIET"}` {
			t.Fatalf("value = %s", out.(*Result).Value)
		}
		if calls := host.recorded(); len(calls) == 0 {
			t.Fatal("the script never reached the host")
		}
	})

	t.Run("args from the ToolsCall", func(t *testing.T) {
		out, _, err := ts.Exec(ctx, time.Now(), "chat history, not an args map", false,
			&taskengine.ToolsCall{Name: ToolsProviderName, ToolName: ToolEval, Args: map[string]string{"code": `"from args"`}})
		if err != nil {
			t.Fatalf("Exec: %v", err)
		}
		if string(out.(*Result).Value) != `"from args"` {
			t.Fatalf("value = %s", out.(*Result).Value)
		}
	})

	t.Run("a string-encoded deadline", func(t *testing.T) {
		out, _, err := ts.Exec(ctx, time.Now(), map[string]any{"code": `1`, "deadline_ms": "250"}, false,
			&taskengine.ToolsCall{Name: ToolsProviderName, ToolName: ToolEval})
		if err != nil {
			t.Fatalf("Exec: %v", err)
		}
		if string(out.(*Result).Value) != "1" {
			t.Fatalf("value = %s", out.(*Result).Value)
		}
	})

	t.Run("the tool name falls back to the provider field", func(t *testing.T) {
		_, _, err := ts.Exec(ctx, time.Now(), map[string]any{"code": `1`}, false,
			&taskengine.ToolsCall{Name: ToolEval})
		if err != nil {
			t.Fatalf("Exec: %v", err)
		}
	})
}

// Every dispatch-time failure (nil call, unknown tool, unknown or missing
// argument, malformed deadline) is refused with a teaching error.
func TestUnit_Tools_ExecRefusals(t *testing.T) {
	ts, _ := newTestToolset(t)
	ctx := context.Background()

	if _, _, err := ts.Exec(ctx, time.Now(), map[string]any{}, false, nil); err == nil ||
		!strings.Contains(err.Error(), "tools required") {
		t.Fatalf("nil call error = %v", err)
	}

	_, dt, err := ts.Exec(ctx, time.Now(), map[string]any{}, false,
		&taskengine.ToolsCall{Name: ToolsProviderName, ToolName: "goja_transpile"})
	if err == nil {
		t.Fatal("an unknown tool ran")
	}
	if dt != taskengine.DataTypeAny {
		t.Fatalf("data type on error = %v", dt)
	}
	for _, name := range []string{ToolEval, "leaker", "echo_upper"} {
		if !strings.Contains(err.Error(), name) {
			t.Errorf("refusal %q does not offer %s", err, name)
		}
	}

	_, _, err = ts.Exec(ctx, time.Now(), map[string]any{"code": `1`, "timeout": 5}, false,
		&taskengine.ToolsCall{Name: ToolsProviderName, ToolName: ToolEval})
	if err == nil || !strings.Contains(err.Error(), "unknown argument(s): timeout") {
		t.Fatalf("unknown-argument error = %v", err)
	}
	if !strings.Contains(err.Error(), "allowed: code, deadline_ms") {
		t.Fatalf("error = %q, want the local_fs unknown-argument shape", err)
	}

	_, _, err = ts.Exec(ctx, time.Now(), map[string]any{"text": "x", "shout": true}, false,
		&taskengine.ToolsCall{Name: ToolsProviderName, ToolName: "echo_upper"})
	if err == nil || !strings.Contains(err.Error(), "unknown argument(s): shout") {
		t.Fatalf("a script's declared schema was not enforced: %v", err)
	}

	_, _, err = ts.Exec(ctx, time.Now(), map[string]any{}, false,
		&taskengine.ToolsCall{Name: ToolsProviderName, ToolName: "echo_upper"})
	if err == nil || !strings.Contains(err.Error(), "missing required argument(s): text") {
		t.Fatalf("missing-required error = %v", err)
	}

	_, _, err = ts.Exec(ctx, time.Now(), map[string]any{}, false,
		&taskengine.ToolsCall{Name: ToolsProviderName, ToolName: ToolEval})
	if err == nil || !strings.Contains(err.Error(), "needs `code`") {
		t.Fatalf("missing-code error = %v", err)
	}

	_, _, err = ts.Exec(ctx, time.Now(), map[string]any{"code": `1`, "deadline_ms": "soon"}, false,
		&taskengine.ToolsCall{Name: ToolsProviderName, ToolName: ToolEval})
	if err == nil || !strings.Contains(err.Error(), "deadline_ms must be a positive number") {
		t.Fatalf("bad-deadline error = %v", err)
	}
}

// EVERY error leaving Exec carries a severity marker, so a model can decide
// whether a corrected retry is worth attempting. Asserted over the whole
// refusal surface rather than case by case, because the value of the
// convention is that it has no holes.
func TestUnit_Tools_EveryExecErrorCarriesASeverityMarker(t *testing.T) {
	ts, _ := newTestToolset(t)
	ctx := context.Background()

	for _, tc := range []struct {
		name  string
		input any
		call  *taskengine.ToolsCall
	}{
		{"nil call", map[string]any{}, nil},
		{"unknown tool", map[string]any{}, &taskengine.ToolsCall{Name: ToolsProviderName, ToolName: "nope"}},
		{"unknown argument", map[string]any{"code": `1`, "x": 1}, &taskengine.ToolsCall{Name: ToolsProviderName, ToolName: ToolEval}},
		{"missing code", map[string]any{}, &taskengine.ToolsCall{Name: ToolsProviderName, ToolName: ToolEval}},
		{"missing required", map[string]any{}, &taskengine.ToolsCall{Name: ToolsProviderName, ToolName: "echo_upper"}},
		{"syntax error", map[string]any{"code": `if (`}, &taskengine.ToolsCall{Name: ToolsProviderName, ToolName: ToolEval}},
		{"a throw", map[string]any{"code": `throw new Error("x")`}, &taskengine.ToolsCall{Name: ToolsProviderName, ToolName: ToolEval}},
		{"a cycle", map[string]any{"code": `const a={};a.s=a;a`}, &taskengine.ToolsCall{Name: ToolsProviderName, ToolName: ToolEval}},
		{"a deadline", map[string]any{"code": `while(true){}`, "deadline_ms": 100}, &taskengine.ToolsCall{Name: ToolsProviderName, ToolName: ToolEval}},
		{"a refused recursion", map[string]any{"code": `host.tool("goja.goja_eval", {})`}, &taskengine.ToolsCall{Name: ToolsProviderName, ToolName: ToolEval}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := ts.Exec(ctx, time.Now(), tc.input, false, tc.call)
			if err == nil {
				t.Fatal("no error")
			}
			msg := err.Error()
			if !strings.Contains(msg, severityRecoverable) && !strings.Contains(msg, severityFatalToken) {
				t.Fatalf("error carries no severity marker: %q", msg)
			}
			if strings.Count(msg, severityRecoverable) > 1 {
				t.Errorf("error carries the marker twice: %q", msg)
			}
			// Nothing model-facing may leak this package's Go internals.
			if strings.Contains(msg, "github.com/contenox") {
				t.Errorf("error leaks a Go symbol path: %q", msg)
			}
		})
	}
}

// Concurrent executions never share a runtime: one script stashes state on
// globalThis, and no other execution ever observes it.
func TestUnit_Tools_ConcurrentExecutionsAreIsolated(t *testing.T) {
	ts, _ := newTestToolset(t)
	ctx := context.Background()

	const workers = 16
	const rounds = 8

	var wg sync.WaitGroup
	errs := make(chan error, workers*rounds*3)

	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for r := 0; r < rounds; r++ {
				out, _, err := ts.Exec(ctx, time.Now(), map[string]any{"tag": fmt.Sprintf("%d-%d", w, r)}, false,
					&taskengine.ToolsCall{Name: ToolsProviderName, ToolName: "leaker"})
				if err != nil {
					errs <- fmt.Errorf("leaker: %w", err)
					continue
				}
				var got struct {
					Count int    `json:"count"`
					Tag   string `json:"tag"`
				}
				if err := json.Unmarshal(out.(*Result).Value, &got); err != nil {
					errs <- err
					continue
				}
				if got.Count != 1 {
					errs <- fmt.Errorf("leaked state: count = %d, want 1 (a previous execution's globalThis was visible)", got.Count)
				}
				if got.Tag != fmt.Sprintf("%d-%d", w, r) {
					errs <- fmt.Errorf("crossed arguments: tag = %q, want %d-%d", got.Tag, w, r)
				}

				out, _, err = ts.Exec(ctx, time.Now(), map[string]any{"code": `typeof globalThis.__leak`}, false,
					&taskengine.ToolsCall{Name: ToolsProviderName, ToolName: ToolEval})
				if err != nil {
					errs <- fmt.Errorf("observer: %w", err)
					continue
				}
				if v := string(out.(*Result).Value); v != `"undefined"` {
					errs <- fmt.Errorf("observed another execution's global: typeof __leak = %s", v)
				}

				out, _, err = ts.Exec(ctx, time.Now(), map[string]any{"text": fmt.Sprintf("w%d", w)}, false,
					&taskengine.ToolsCall{Name: ToolsProviderName, ToolName: "echo_upper"})
				if err != nil {
					errs <- fmt.Errorf("echo: %w", err)
					continue
				}
				want := fmt.Sprintf(`{"echoed":"W%d"}`, w)
				if got := string(out.(*Result).Value); got != want {
					errs <- fmt.Errorf("crossed host results: %s, want %s", got, want)
				}
			}
		}(w)
	}
	wg.Wait()
	close(errs)

	failures := 0
	for err := range errs {
		failures++
		if failures <= 10 {
			t.Error(err)
		}
	}
	if failures > 10 {
		t.Errorf("... and %d more", failures-10)
	}
}

// After Shutdown, Exec is refused but Supports still answers.
func TestUnit_Tools_ShutdownStopsTheProvider(t *testing.T) {
	ts, _ := newTestToolset(t)
	ctx := context.Background()

	if _, _, err := ts.Exec(ctx, time.Now(), map[string]any{"code": `1`}, false,
		&taskengine.ToolsCall{Name: ToolsProviderName, ToolName: ToolEval}); err != nil {
		t.Fatalf("pre-shutdown Exec: %v", err)
	}

	ts.Shutdown()

	_, _, err := ts.Exec(ctx, time.Now(), map[string]any{"code": `1`}, false,
		&taskengine.ToolsCall{Name: ToolsProviderName, ToolName: ToolEval})
	if err == nil || !strings.Contains(err.Error(), "shut down") {
		t.Fatalf("post-shutdown Exec error = %v, want a shutdown refusal", err)
	}

	if names, err := ts.Supports(ctx); err != nil || len(names) == 0 {
		t.Fatalf("Supports after shutdown = %v, %v", names, err)
	}
}

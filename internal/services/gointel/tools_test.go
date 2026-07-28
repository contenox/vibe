package gointel

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/contenox/contenox/internal/kernel/taskengine"
)

func newTestTools(t *testing.T) (taskengine.ToolsRepo, *index) {
	t.Helper()
	ix := newTestIndex(t, newFixture(t, "fixture"))
	return NewTools(ix), ix
}

func TestUnit_Tools_SupportsNamesTheProviderAndSixTools(t *testing.T) {
	repo, _ := newTestTools(t)

	got, err := repo.Supports(context.Background())
	if err != nil {
		t.Fatalf("Supports: %v", err)
	}
	want := []string{
		ToolsProviderName,
		"go_describe", "go_definition", "go_references",
		"go_implementations", "go_symbols", "go_diagnostics",
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("Supports() = %v, want %v", got, want)
	}
	// The names are the HITL policy key.
	if ToolsProviderName != "gointel" {
		t.Fatalf("provider name = %q", ToolsProviderName)
	}
}

func TestUnit_Tools_SchemaShape(t *testing.T) {
	repo, _ := newTestTools(t)
	ctx := context.Background()

	all, err := repo.GetToolsForToolsByName(ctx, ToolsProviderName)
	if err != nil {
		t.Fatalf("GetToolsForToolsByName: %v", err)
	}
	if len(all) != 6 {
		t.Fatalf("%d tools, want 6", len(all))
	}

	byName := map[string]taskengine.Tool{}
	for _, tool := range all {
		if tool.Type != "function" {
			t.Errorf("%s type = %q", tool.Function.Name, tool.Type)
		}
		byName[tool.Function.Name] = tool
	}

	for _, name := range []string{ToolDescribe, ToolDefinition, ToolReferences, ToolImplementations, ToolSymbols, ToolDiagnostics} {
		tool, ok := byName[name]
		if !ok {
			t.Fatalf("%s missing from the tool list", name)
		}
		desc := tool.Function.Description
		// Build-context defaults cannot be re-taught by an error that never
		// fires, so they must be in the schema.
		for _, want := range []string{"GOOS/GOARCH", "no build tags", "tests excluded"} {
			if !strings.Contains(desc, want) {
				t.Errorf("%s description does not state %q", name, want)
			}
		}
		params, ok := tool.Function.Parameters.(map[string]any)
		if !ok {
			t.Fatalf("%s parameters are %T, want a JSON Schema object", name, tool.Function.Parameters)
		}
		props, ok := params["properties"].(map[string]any)
		if !ok {
			t.Fatalf("%s has no properties", name)
		}
		if _, ok := props["dir"]; !ok {
			t.Errorf("%s does not take dir", name)
		}

		// Individually addressable, exactly like local_fs.
		one, err := repo.GetToolsForToolsByName(ctx, name)
		if err != nil || len(one) != 1 || one[0].Function.Name != name {
			t.Errorf("GetToolsForToolsByName(%q) = %v, %v", name, one, err)
		}
	}

	if !strings.Contains(byName[ToolDiagnostics].Function.Description, "ADVISORY") {
		t.Error("go_diagnostics does not carry the advisory framing")
	}
	diagProps := byName[ToolDiagnostics].Function.Parameters.(map[string]any)["properties"].(map[string]any)
	passesDesc := diagProps["passes"].(map[string]any)["description"].(string)
	for _, name := range VetPasses() {
		if !strings.Contains(passesDesc, name) {
			t.Errorf("passes description does not offer %s", name)
		}
	}

	for _, name := range []string{ToolDescribe, ToolDefinition, ToolReferences, ToolImplementations} {
		params := byName[name].Function.Parameters.(map[string]any)
		req, _ := params["required"].([]string)
		if len(req) != 1 || req[0] != "symbol" {
			t.Errorf("%s required = %v, want [symbol]", name, req)
		}
	}

	if _, err := repo.GetToolsForToolsByName(ctx, "go_rename"); err == nil {
		t.Error("an unknown tool name resolved")
	}
}

func TestUnit_Tools_NoOpenAPISchemas(t *testing.T) {
	repo, _ := newTestTools(t)

	got, err := repo.GetSchemasForSupportedTools(context.Background())
	if err != nil {
		t.Fatalf("GetSchemasForSupportedTools: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("got %d OpenAPI documents, want none (hand-written function schemas, as in local_fs)", len(got))
	}
}

func TestUnit_Tools_ExecDispatchesEveryTool(t *testing.T) {
	repo, _ := newTestTools(t)
	ctx := context.Background()

	for _, tc := range []struct {
		tool string
		args map[string]any
		want func(any) error
	}{
		{ToolDefinition, map[string]any{"symbol": "shapes.Rect"}, func(v any) error {
			res := v.(*DefinitionResult)
			if res.Location != "shapes/shapes.go:25:6" {
				t.Errorf("location = %q", res.Location)
			}
			return nil
		}},
		{ToolDescribe, map[string]any{"symbol": "shapes.Rect"}, func(v any) error {
			if len(v.(*DescribeResult).Fields) != 2 {
				t.Error("describe lost the fields")
			}
			return nil
		}},
		{ToolReferences, map[string]any{"symbol": "shapes.Unit", "max": "2"}, func(v any) error {
			res := v.(*ReferencesResult)
			// "2" as a string: small models emit JSON scalars as strings.
			if res.Shown != 2 {
				t.Errorf("shown = %d, want the string-encoded max to be honoured", res.Shown)
			}
			return nil
		}},
		{ToolImplementations, map[string]any{"symbol": "shapes.Shape"}, func(v any) error {
			if len(v.(*ImplementationsResult).Implementers) != 2 {
				t.Error("implementations lost an implementer")
			}
			return nil
		}},
		{ToolSymbols, map[string]any{"target": "shapes"}, func(v any) error {
			if v.(*SymbolsResult).Total != 13 {
				t.Error("symbols outline changed")
			}
			return nil
		}},
		{ToolDiagnostics, map[string]any{"scope": "all", "passes": "printf, unreachable"}, func(v any) error {
			res := v.(*DiagnosticsResult)
			if res.Toolchain == "" {
				t.Error("diagnostics carried no toolchain view")
			}
			// Comma-separated string, the shape a model actually emits for a list.
			if strings.Join(res.Passes, ",") != "printf,unreachable" {
				t.Errorf("passes = %v", res.Passes)
			}
			return nil
		}},
	} {
		t.Run(tc.tool, func(t *testing.T) {
			out, dt, err := repo.Exec(ctx, time.Now(), tc.args, false,
				&taskengine.ToolsCall{Name: ToolsProviderName, ToolName: tc.tool})
			if err != nil {
				t.Fatalf("Exec: %v", err)
			}
			if dt != taskengine.DataTypeJSON {
				t.Fatalf("data type = %v, want JSON", dt)
			}
			_ = tc.want(out)
		})
	}
}

func TestUnit_Tools_ExecReadsArgsFromTheToolsCall(t *testing.T) {
	repo, _ := newTestTools(t)

	// A declarative `tools` task carries its arguments on the call, not the
	// chain input — the same fallback local_fs implements.
	out, _, err := repo.Exec(context.Background(), time.Now(), "chat history not an args map", false,
		&taskengine.ToolsCall{Name: ToolsProviderName, ToolName: ToolDefinition, Args: map[string]string{"symbol": "shapes.Rect"}})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if out.(*DefinitionResult).Symbol != "example.com/fixture/shapes.Rect" {
		t.Fatalf("symbol = %q", out.(*DefinitionResult).Symbol)
	}
}

func TestUnit_Tools_ExecRejectsUnknownArguments(t *testing.T) {
	repo, _ := newTestTools(t)

	_, _, err := repo.Exec(context.Background(), time.Now(),
		map[string]any{"symbol": "shapes.Rect", "line": 12}, false,
		&taskengine.ToolsCall{Name: ToolsProviderName, ToolName: ToolDefinition})
	if err == nil {
		t.Fatal("an unknown argument was accepted")
	}
	if !strings.Contains(err.Error(), "unknown argument(s): line") || !strings.Contains(err.Error(), "allowed: dir, symbol") {
		t.Fatalf("error = %q, want the local_fs unknown-argument shape", err)
	}
}

func TestUnit_Tools_ExecRefusals(t *testing.T) {
	repo, _ := newTestTools(t)
	ctx := context.Background()

	if _, _, err := repo.Exec(ctx, time.Now(), map[string]any{}, false, nil); err == nil ||
		!strings.Contains(err.Error(), "tools required") {
		t.Fatalf("nil call error = %v", err)
	}

	_, _, err := repo.Exec(ctx, time.Now(), map[string]any{}, false,
		&taskengine.ToolsCall{Name: ToolsProviderName, ToolName: "go_rename"})
	if err == nil || !strings.Contains(err.Error(), "unknown tool") {
		t.Fatalf("unknown tool error = %v", err)
	}
	// The refusal must list what is available.
	for _, name := range toolNames {
		if !strings.Contains(err.Error(), name) {
			t.Errorf("refusal %q does not offer %s", err, name)
		}
	}
}

func TestUnit_Tools_ExecSurfacesResolutionErrors(t *testing.T) {
	repo, _ := newTestTools(t)

	_, dt, err := repo.Exec(context.Background(), time.Now(),
		map[string]any{"symbol": "Unit"}, false,
		&taskengine.ToolsCall{Name: ToolsProviderName, ToolName: ToolDefinition})
	if err == nil {
		t.Fatal("an ambiguous symbol resolved through the tool surface")
	}
	if dt != taskengine.DataTypeAny {
		t.Fatalf("data type on error = %v", dt)
	}
	if !strings.Contains(err.Error(), "re-ask with one of these qualified names") {
		t.Fatalf("error = %q", err)
	}
}

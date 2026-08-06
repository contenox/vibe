package gointel

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/contenox/contenox/internal/kernel/taskengine"
	"github.com/getkin/kin-openapi/openapi3"
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

// TestUnit_Tools_PublishedSchemaMatchesToolDescriptors pins the declared
// OpenAPI contract and its agreement with what actually reaches the provider:
// every tool has a request and a response schema, and each request schema
// carries exactly the descriptor's properties — same types, same descriptions,
// same required set — since both are rendered from one table (goToolSpecs).
func TestUnit_Tools_PublishedSchemaMatchesToolDescriptors(t *testing.T) {
	repo, _ := newTestTools(t)
	ctx := context.Background()

	docs, err := repo.GetSchemasForSupportedTools(ctx)
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

	// The component name is part of the contract: a rename is a breaking change.
	components := map[string]string{
		ToolDescribe:        "GoDescribe",
		ToolDefinition:      "GoDefinition",
		ToolReferences:      "GoReferences",
		ToolImplementations: "GoImplementations",
		ToolSymbols:         "GoSymbols",
		ToolDiagnostics:     "GoDiagnostics",
	}
	if len(doc.Components.Schemas) != 2*len(components) {
		t.Errorf("%d component schemas, want a request and a response for each of the %d tools", len(doc.Components.Schemas), len(components))
	}

	for _, name := range toolNames {
		component, ok := components[name]
		if !ok {
			t.Fatalf("%s declares no OpenAPI component prefix", name)
		}
		req := doc.Components.Schemas[component+"Request"]
		if req == nil || req.Value == nil {
			t.Fatalf("%s: no %sRequest schema is published", name, component)
		}
		resp := doc.Components.Schemas[component+"Response"]
		if resp == nil || resp.Value == nil {
			t.Fatalf("%s: no %sResponse schema is published", name, component)
		}

		declared, err := repo.GetToolsForToolsByName(ctx, name)
		if err != nil || len(declared) != 1 {
			t.Fatalf("GetToolsForToolsByName(%q) = %v, %v", name, declared, err)
		}
		params := declared[0].Function.Parameters.(map[string]any)
		props := params["properties"].(map[string]any)
		if len(props) != len(req.Value.Properties) {
			t.Errorf("%s: descriptor declares %d properties, the published schema %d", name, len(props), len(req.Value.Properties))
		}
		for prop, published := range req.Value.Properties {
			declaredProp, ok := props[prop].(map[string]any)
			if !ok {
				t.Errorf("%s: %s is published but the descriptor does not declare it", name, prop)
				continue
			}
			if got, want := publishedTypes(published), declaredTypes(declaredProp["type"]); strings.Join(got, ",") != strings.Join(want, ",") {
				t.Errorf("%s.%s: published type %v, descriptor type %v", name, prop, got, want)
			}
			if published.Value.Description == "" {
				t.Errorf("%s.%s: published without a description", name, prop)
			}
			if published.Value.Description != declaredProp["description"] {
				t.Errorf("%s.%s: descriptor and published schema disagree on the description", name, prop)
			}
			// A closed value set is part of the contract in both places, and a
			// union's set sits on the array branch's items in both places.
			if got, want := enumOf(published.Value), declaredEnum(declaredProp["enum"]); strings.Join(got, ",") != strings.Join(want, ",") {
				t.Errorf("%s.%s: published enum %v, descriptor enum %v", name, prop, got, want)
			}
			declaredItems, hasItems := declaredProp["items"].(map[string]any)
			if hasItems != (published.Value.Items != nil) {
				t.Errorf("%s.%s: descriptor declares items=%v, published schema items=%v", name, prop, hasItems, published.Value.Items != nil)
				continue
			}
			if !hasItems {
				continue
			}
			if got, want := publishedTypes(published.Value.Items), declaredTypes(declaredItems["type"]); strings.Join(got, ",") != strings.Join(want, ",") {
				t.Errorf("%s.%s items: published type %v, descriptor type %v", name, prop, got, want)
			}
			if got, want := enumOf(published.Value.Items.Value), declaredEnum(declaredItems["enum"]); strings.Join(got, ",") != strings.Join(want, ",") {
				t.Errorf("%s.%s items: published enum %v, descriptor enum %v", name, prop, got, want)
			}
		}
		// A tool with no required argument declares none in either place.
		wantRequired, _ := params["required"].([]string)
		if strings.Join(wantRequired, ",") != strings.Join(req.Value.Required, ",") {
			t.Errorf("%s: required = %v, descriptor requires %v", name, req.Value.Required, wantRequired)
		}

		// The response contract describes the payload Exec returns.
		if len(resp.Value.Properties) == 0 {
			t.Errorf("%s: the response schema declares no properties", name)
		}
		if resp.Value.Properties["toolchain"] == nil {
			t.Errorf("%s: the response schema omits toolchain, which every result carries", name)
		}
		for prop, published := range resp.Value.Properties {
			if published.Value.Description == "" {
				t.Errorf("%s response: %s is published without a description", name, prop)
			}
		}
	}
}

// publishedTypes reads a published schema's type set, which is one entry for an
// ordinary argument and several for a union.
func publishedTypes(ref *openapi3.SchemaRef) []string {
	if ref == nil || ref.Value == nil || ref.Value.Type == nil {
		return nil
	}
	return append([]string(nil), *ref.Value.Type...)
}

// declaredTypes reads the descriptor's "type", which JSON Schema spells as a
// bare string for one type and a list for a union.
func declaredTypes(v any) []string {
	switch t := v.(type) {
	case string:
		return []string{t}
	case []any:
		out := make([]string, 0, len(t))
		for _, item := range t {
			out = append(out, item.(string))
		}
		return out
	}
	return nil
}

func enumOf(s *openapi3.Schema) []string {
	if s == nil {
		return nil
	}
	out := make([]string, 0, len(s.Enum))
	for _, v := range s.Enum {
		out = append(out, v.(string))
	}
	return out
}

func declaredEnum(v any) []string {
	values, ok := v.([]string)
	if !ok {
		return nil
	}
	return append([]string(nil), values...)
}

// TestUnit_Tools_ClosedValueSetsAreDeclared pins the two go_diagnostics
// arguments whose legal values the implementation closes: scope, refused by
// Diagnostics outside its three, and passes, refused by resolvePasses outside
// the curated set plus "all". passes also carries the type union argStrings
// accepts, since a descriptor narrower than Exec is the same defect as a
// value set stated only in prose.
func TestUnit_Tools_ClosedValueSetsAreDeclared(t *testing.T) {
	repo, _ := newTestTools(t)
	ctx := context.Background()

	declared, err := repo.GetToolsForToolsByName(ctx, ToolDiagnostics)
	if err != nil {
		t.Fatalf("GetToolsForToolsByName: %v", err)
	}
	props := declared[0].Function.Parameters.(map[string]any)["properties"].(map[string]any)

	scope := props["scope"].(map[string]any)
	if got := declaredEnum(scope["enum"]); strings.Join(got, ",") != strings.Join([]string{ScopeChanged, ScopePackage, ScopeAll}, ",") {
		t.Errorf("scope enum = %v, want the three scopes Diagnostics accepts", got)
	}

	passes := props["passes"].(map[string]any)
	if got := declaredTypes(passes["type"]); strings.Join(got, ",") != "string,array" {
		t.Errorf("passes type = %v, want the string|array union argStrings accepts", got)
	}
	items, ok := passes["items"].(map[string]any)
	if !ok {
		t.Fatalf("passes declares an array branch with no items: %v", passes)
	}
	if items["type"] != "string" {
		t.Errorf("passes items type = %v, want string", items["type"])
	}
	wantPasses := append(VetPasses(), "all")
	if got := declaredEnum(items["enum"]); strings.Join(got, ",") != strings.Join(wantPasses, ",") {
		t.Errorf("passes items enum = %v, want %v", got, wantPasses)
	}

	// The declared union is the one Exec honours: both branches reach the same
	// pass set, and a name outside the enum is refused rather than dropped.
	for _, form := range []any{"printf,unreachable", []any{"printf", "unreachable"}, []string{"printf", "unreachable"}} {
		res, err := execTool(t, repo, ToolDiagnostics, map[string]any{"scope": "all", "passes": form})
		if err != nil {
			t.Fatalf("passes=%#v: %v", form, err)
		}
		if got := strings.Join(res.(*DiagnosticsResult).Passes, ","); got != "printf,unreachable" {
			t.Errorf("passes=%#v ran %q", form, got)
		}
	}
	if _, err := execTool(t, repo, ToolDiagnostics, map[string]any{"scope": "all", "passes": []any{"nosuchpass"}}); err == nil {
		t.Error("a pass outside the declared enum was accepted")
	}

	// max stays an integer in the schema on every tool that takes one; the
	// string tolerance argInt applies is stated in the description instead.
	for _, tool := range []string{ToolReferences, ToolSymbols, ToolDiagnostics} {
		one, err := repo.GetToolsForToolsByName(ctx, tool)
		if err != nil {
			t.Fatalf("GetToolsForToolsByName(%q): %v", tool, err)
		}
		maxProp, ok := one[0].Function.Parameters.(map[string]any)["properties"].(map[string]any)["max"].(map[string]any)
		if !ok {
			t.Fatalf("%s declares no max", tool)
		}
		if maxProp["type"] != "integer" {
			t.Errorf("%s.max type = %v, want integer", tool, maxProp["type"])
		}
		if !strings.Contains(maxProp["description"].(string), "decimal string") {
			t.Errorf("%s.max description does not state the string tolerance argInt applies: %q", tool, maxProp["description"])
		}
	}
	// And Exec really honours it, so the sentence is not a promise the code breaks.
	syms, err := execTool(t, repo, ToolSymbols, map[string]any{"target": "shapes", "max": "1"})
	if err != nil {
		t.Fatalf("max=\"1\": %v", err)
	}
	if got := syms.(*SymbolsResult); got.Shown != 1 || got.Total < 2 {
		t.Errorf("max=\"1\" returned shown=%d of total=%d; the string form was not read as the number", got.Shown, got.Total)
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

package shellsession

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// TestUnit_Tools_PublishedSchemaMatchesToolDescriptors pins that the OpenAPI contract and the tool descriptors agree property for property, since both are rendered from one table (shellToolSpecs).
func TestUnit_Tools_PublishedSchemaMatchesToolDescriptors(t *testing.T) {
	repo := NewTools(nil)
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

	components := map[string]string{
		ToolRun:  "ShellSessionRun",
		ToolRead: "ShellSessionRead",
	}
	if len(doc.Components.Schemas) != 2*len(components) {
		t.Errorf("%d component schemas, want a request and a response for each of the %d tools", len(doc.Components.Schemas), len(components))
	}

	for _, name := range []string{ToolRun, ToolRead} {
		component := components[name]
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
			if published.Value.Type == nil || !published.Value.Type.Is(declaredProp["type"].(string)) {
				t.Errorf("%s.%s: published type %v, descriptor type %v", name, prop, published.Value.Type, declaredProp["type"])
			}
			if published.Value.Description == "" {
				t.Errorf("%s.%s: published without a description", name, prop)
			}
			if published.Value.Description != declaredProp["description"] {
				t.Errorf("%s.%s: descriptor and published schema disagree on the description", name, prop)
			}
			if len(published.Value.Enum) != 0 {
				t.Errorf("%s.%s: published an enum the descriptor does not declare", name, prop)
			}
		}
		wantRequired, _ := params["required"].([]string)
		if strings.Join(wantRequired, ",") != strings.Join(req.Value.Required, ",") {
			t.Errorf("%s: required = %v, descriptor requires %v", name, req.Value.Required, wantRequired)
		}

		if len(resp.Value.Properties) == 0 {
			t.Errorf("%s: the response schema declares no properties", name)
		}
		for prop, published := range resp.Value.Properties {
			if published.Value.Description == "" {
				t.Errorf("%s response: %s is published without a description", name, prop)
			}
			if published.Value.Type == nil {
				t.Errorf("%s response: %s is published without a type", name, prop)
			}
		}
	}

	// Constructed here rather than run through a real shell, so the contract is pinned on every platform.
	for _, tc := range []struct {
		component string
		payload   any
	}{
		{"ShellSessionRun", RunResultJSON{Offset: 12, Output: "hello", Started: true, Note: "n"}},
		{"ShellSessionRead", ReadResultJSON{Content: "hello", FromOffset: 0, NextOffset: 12, Exists: true, Note: "n"}},
	} {
		resp := doc.Components.Schemas[tc.component+"Response"]
		raw, err := json.Marshal(tc.payload)
		if err != nil {
			t.Fatalf("%s: marshal payload: %v", tc.component, err)
		}
		var got map[string]any
		if err := json.Unmarshal(raw, &got); err != nil {
			t.Fatalf("%s: unmarshal payload: %v", tc.component, err)
		}
		for prop := range got {
			if _, ok := resp.Value.Properties[prop]; !ok {
				t.Errorf("%s: the result carries %s but the published schema does not declare it", tc.component, prop)
			}
		}
		for _, prop := range resp.Value.Required {
			if _, ok := got[prop]; !ok {
				t.Errorf("%s: the published schema requires %s but the result omits it", tc.component, prop)
			}
		}
	}
}

// TestUnit_Tools_DescriptorsAreAddressableByName pins that rendering the
// descriptors from the spec table left the provider's lookup behavior intact.
func TestUnit_Tools_DescriptorsAreAddressableByName(t *testing.T) {
	repo := NewTools(nil)
	ctx := context.Background()

	all, err := repo.GetToolsForToolsByName(ctx, ToolsProviderName)
	if err != nil || len(all) != 2 {
		t.Fatalf("GetToolsForToolsByName(provider) = %d tools, %v", len(all), err)
	}
	if all[0].Function.Name != ToolRun || all[1].Function.Name != ToolRead {
		t.Errorf("declaration order = %s, %s", all[0].Function.Name, all[1].Function.Name)
	}
	for _, tool := range all {
		if tool.Type != "function" || tool.Function.Description == "" {
			t.Errorf("%s is not a described function tool: %+v", tool.Function.Name, tool)
		}
	}
	for _, name := range []string{ToolRun, ToolRead, ""} {
		if _, err := repo.GetToolsForToolsByName(ctx, name); err != nil {
			t.Errorf("GetToolsForToolsByName(%q): %v", name, err)
		}
	}
	if _, err := repo.GetToolsForToolsByName(ctx, "shell_session_kill"); err == nil {
		t.Error("an unknown tool name resolved")
	}
	read, _ := repo.GetToolsForToolsByName(ctx, ToolRead)
	if _, ok := read[0].Function.Parameters.(map[string]any)["required"]; ok {
		t.Error("shell_session_read must declare no required key at all")
	}
}

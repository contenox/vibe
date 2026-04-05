package main

import (
	"context"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/contenox/contenox/apiframework"
	"github.com/contenox/contenox/tools/openapi-gen/internal/project"
	"github.com/contenox/contenox/tools/openapi-gen/internal/routes"
	"github.com/contenox/contenox/tools/openapi-gen/internal/schema"
	"github.com/getkin/kin-openapi/openapi3"
)

func TestGeneratedSpecIsValidAndClientSafe(t *testing.T) {
	root := repoRoot(t)
	tempDir := t.TempDir()

	fset := token.NewFileSet()
	pkgs, err := project.LoadPackages(fset, root)
	if err != nil {
		t.Fatalf("load packages: %v", err)
	}

	swagger := schema.NewDocument(apiframework.GetVersion())
	routes.Process(fset, pkgs, swagger)
	schema.AddTypes(swagger, pkgs)
	schema.Finalize(swagger)

	jsonPath, _, err := writeOutputs(swagger, tempDir)
	if err != nil {
		t.Fatalf("write outputs: %v", err)
	}
	if err := validateOutput(jsonPath); err != nil {
		t.Fatalf("validate output: %v", err)
	}

	loader := openapi3.NewLoader()
	doc, err := loader.LoadFromFile(jsonPath)
	if err != nil {
		t.Fatalf("reload json: %v", err)
	}
	if err := doc.Validate(context.Background()); err != nil {
		t.Fatalf("validate with kin-openapi: %v", err)
	}

	if missing := findMissingSchemaRefs(doc); len(missing) > 0 {
		t.Fatalf("missing schema refs: %v", missing)
	}

	assertResponseRef(t, doc, "/version", "get", "200", "#/components/schemas/apiframework_AboutServer")
	assertRequestRef(t, doc, "/plans", "post", "#/components/schemas/planapi_newPlanRequest")
	assertResponseRef(t, doc, "/plans", "post", "201", "#/components/schemas/planapi_newPlanResponse")
	assertResponseRef(t, doc, "/taskchains", "get", "200", "#/components/schemas/taskengine_TaskChainDefinition")
	assertRequestRef(t, doc, "/taskchains", "post", "#/components/schemas/taskengine_TaskChainDefinition")
	assertResponseRef(t, doc, "/taskchains", "post", "201", "#/components/schemas/taskengine_TaskChainDefinition")
	assertRequestRef(t, doc, "/chats/{id}/chat", "post", "#/components/schemas/internalchatapi_chatRequest")
	assertResponseRef(t, doc, "/chats/{id}/chat", "post", "200", "#/components/schemas/internalchatapi_chatResponse")
	assertRequestRef(t, doc, "/execute", "post", "#/components/schemas/execservice_TaskRequest")
	assertResponseRef(t, doc, "/execute", "post", "200", "#/components/schemas/execservice_SimpleExecutionResponse")

	assertParameterDescriptions(t, doc)
	assertOnlyAllowedInlineObjectResponses(t, doc)
}

func repoRoot(t *testing.T) string {
	t.Helper()

	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	return filepath.Clean(filepath.Join(wd, "..", ".."))
}

func assertRequestRef(t *testing.T, doc *openapi3.T, path, method, wantRef string) {
	t.Helper()
	op := mustOperation(t, doc, path, method)
	if op.RequestBody == nil || op.RequestBody.Value == nil {
		t.Fatalf("%s %s missing request body", method, path)
	}
	got := op.RequestBody.Value.Content.Get("application/json")
	if got == nil || got.Schema == nil {
		t.Fatalf("%s %s missing application/json request schema", method, path)
	}
	if got.Schema.Ref != wantRef {
		t.Fatalf("%s %s request ref = %q, want %q", method, path, got.Schema.Ref, wantRef)
	}
}

func assertResponseRef(t *testing.T, doc *openapi3.T, path, method, status, wantRef string) {
	t.Helper()
	op := mustOperation(t, doc, path, method)
	resp := op.Responses.Map()[status]
	if resp == nil || resp.Value == nil {
		t.Fatalf("%s %s missing response %s", method, path, status)
	}
	got := resp.Value.Content.Get("application/json")
	if got == nil || got.Schema == nil {
		t.Fatalf("%s %s response %s missing application/json schema", method, path, status)
	}
	if got.Schema.Ref != wantRef {
		t.Fatalf("%s %s response %s ref = %q, want %q", method, path, status, got.Schema.Ref, wantRef)
	}
}

func mustOperation(t *testing.T, doc *openapi3.T, path, method string) *openapi3.Operation {
	t.Helper()
	item := doc.Paths.Find(path)
	if item == nil {
		t.Fatalf("missing path %s", path)
	}
	switch method {
	case "get":
		if item.Get == nil {
			t.Fatalf("missing GET %s", path)
		}
		return item.Get
	case "post":
		if item.Post == nil {
			t.Fatalf("missing POST %s", path)
		}
		return item.Post
	case "put":
		if item.Put == nil {
			t.Fatalf("missing PUT %s", path)
		}
		return item.Put
	case "delete":
		if item.Delete == nil {
			t.Fatalf("missing DELETE %s", path)
		}
		return item.Delete
	default:
		t.Fatalf("unsupported method %s", method)
		return nil
	}
}

func assertParameterDescriptions(t *testing.T, doc *openapi3.T) {
	t.Helper()
	for path, item := range doc.Paths.Map() {
		for _, p := range item.Parameters {
			assertParamHasDescription(t, path, "path-item", p)
		}
		for method, op := range map[string]*openapi3.Operation{
			"get":    item.Get,
			"post":   item.Post,
			"put":    item.Put,
			"patch":  item.Patch,
			"delete": item.Delete,
		} {
			if op == nil {
				continue
			}
			for _, p := range op.Parameters {
				assertParamHasDescription(t, path, method, p)
			}
		}
	}
}

func assertParamHasDescription(t *testing.T, path, method string, p *openapi3.ParameterRef) {
	t.Helper()
	if p == nil || p.Value == nil {
		t.Fatalf("%s %s has nil parameter", method, path)
	}
	if p.Value.In == "path" || p.Value.In == "query" {
		if strings.TrimSpace(p.Value.Description) == "" {
			t.Fatalf("%s %s parameter %q is missing a description", method, path, p.Value.Name)
		}
	}
}

func assertOnlyAllowedInlineObjectResponses(t *testing.T, doc *openapi3.T) {
	t.Helper()
	allowed := map[string]struct{}{
		"GET /hooks/schemas 200": {},
	}
	for path, item := range doc.Paths.Map() {
		for method, op := range map[string]*openapi3.Operation{
			"GET":    item.Get,
			"POST":   item.Post,
			"PUT":    item.Put,
			"PATCH":  item.Patch,
			"DELETE": item.Delete,
		} {
			if op == nil {
				continue
			}
			for status, resp := range op.Responses.Map() {
				if resp == nil || resp.Value == nil {
					continue
				}
				mt := resp.Value.Content.Get("application/json")
				if mt == nil || mt.Schema == nil || mt.Schema.Ref != "" || mt.Schema.Value == nil {
					continue
				}
				if mt.Schema.Value.Type.Is("object") {
					key := method + " " + path + " " + status
					if _, ok := allowed[key]; !ok {
						t.Fatalf("unexpected inline object response at %s", key)
					}
				}
			}
		}
	}
}

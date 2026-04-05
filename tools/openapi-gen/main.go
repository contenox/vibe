package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"go/token"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/contenox/contenox/apiframework"
	"github.com/contenox/contenox/tools/openapi-gen/internal/project"
	"github.com/contenox/contenox/tools/openapi-gen/internal/routes"
	"github.com/contenox/contenox/tools/openapi-gen/internal/schema"
	"github.com/getkin/kin-openapi/openapi3"
	"gopkg.in/yaml.v3"
)

func main() {
	var projectDir string
	var outputDir string

	flag.StringVar(&projectDir, "project", "", "The root directory of the Go project to parse.")
	flag.StringVar(&outputDir, "output", "docs", "The output directory for the generated OpenAPI spec.")
	flag.Parse()

	if projectDir == "" {
		fmt.Println("Error: The --project flag is required.")
		flag.Usage()
		os.Exit(1)
	}

	fset := tokenFileSet()
	pkgs, err := project.LoadPackages(fset, projectDir)
	if err != nil {
		log.Fatal("Failed to parse project:", err)
	}

	swagger := schema.NewDocument(apiframework.GetVersion())
	routes.Process(fset, pkgs, swagger)
	schema.AddTypes(swagger, pkgs)
	schema.Finalize(swagger)

	jsonPath, yamlPath, err := writeOutputs(swagger, outputDir)
	if err != nil {
		log.Fatal(err)
	}
	if err := validateOutput(jsonPath); err != nil {
		log.Fatal(err)
	}

	fmt.Printf("✅ OpenAPI spec generated at %s\n", yamlPath)
}

func tokenFileSet() *token.FileSet {
	return token.NewFileSet()
}

func writeOutputs(swagger *openapi3.T, outputDir string) (string, string, error) {
	data, err := json.MarshalIndent(swagger, "", "  ")
	if err != nil {
		return "", "", fmt.Errorf("marshal json: %w", err)
	}

	if err := os.MkdirAll(outputDir, 0o755); err != nil {
		return "", "", fmt.Errorf("mkdir output: %w", err)
	}

	jsonPath := filepath.Join(outputDir, "openapi.json")
	if err := os.WriteFile(jsonPath, data, 0o644); err != nil {
		return "", "", fmt.Errorf("write %s: %w", jsonPath, err)
	}

	yamlData, err := yaml.Marshal(swagger)
	if err != nil {
		return "", "", fmt.Errorf("marshal yaml: %w", err)
	}

	yamlPath := filepath.Join(outputDir, "openapi.yaml")
	if err := os.WriteFile(yamlPath, yamlData, 0o644); err != nil {
		return "", "", fmt.Errorf("write %s: %w", yamlPath, err)
	}

	return jsonPath, yamlPath, nil
}

func validateOutput(jsonPath string) error {
	loader := openapi3.NewLoader()
	doc, err := loader.LoadFromFile(jsonPath)
	if err != nil {
		return fmt.Errorf("load generated spec: %w", err)
	}
	if err := doc.Validate(context.Background()); err != nil {
		return fmt.Errorf("validate generated spec: %w", err)
	}
	if missing := findMissingSchemaRefs(doc); len(missing) > 0 {
		return fmt.Errorf("generated spec has unresolved schema refs: %s", strings.Join(missing, ", "))
	}
	return nil
}

func findMissingSchemaRefs(doc *openapi3.T) []string {
	schemas := map[string]struct{}{}
	if doc.Components != nil {
		for name := range doc.Components.Schemas {
			schemas[name] = struct{}{}
		}
	}

	refs := map[string]struct{}{}
	walkRefs(doc.Paths.Map(), refs)

	missing := make([]string, 0)
	for name := range refs {
		if _, ok := schemas[name]; !ok {
			missing = append(missing, name)
		}
	}
	sort.Strings(missing)
	return missing
}

func walkRefs(v any, refs map[string]struct{}) {
	switch value := v.(type) {
	case map[string]*openapi3.PathItem:
		for _, item := range value {
			walkRefs(item, refs)
		}
	case *openapi3.PathItem:
		if value == nil {
			return
		}
		for _, op := range []*openapi3.Operation{value.Get, value.Put, value.Post, value.Delete, value.Options, value.Head, value.Patch, value.Trace} {
			walkRefs(op, refs)
		}
	case *openapi3.Operation:
		if value == nil {
			return
		}
		for _, p := range value.Parameters {
			walkRefs(p, refs)
		}
		walkRefs(value.RequestBody, refs)
		if value.Responses != nil {
			for _, response := range value.Responses.Map() {
				walkRefs(response, refs)
			}
		}
	case *openapi3.RequestBodyRef:
		if value == nil || value.Value == nil {
			return
		}
		for _, mediaType := range value.Value.Content {
			walkRefs(mediaType, refs)
		}
	case *openapi3.ResponseRef:
		if value == nil || value.Value == nil {
			return
		}
		for _, mediaType := range value.Value.Content {
			walkRefs(mediaType, refs)
		}
	case *openapi3.MediaType:
		if value == nil || value.Schema == nil {
			return
		}
		walkRefs(value.Schema, refs)
	case *openapi3.SchemaRef:
		if value == nil {
			return
		}
		if ref := value.Ref; strings.HasPrefix(ref, "#/components/schemas/") {
			refs[strings.TrimPrefix(ref, "#/components/schemas/")] = struct{}{}
		}
		if value.Value != nil {
			for _, prop := range value.Value.Properties {
				walkRefs(prop, refs)
			}
			walkRefs(value.Value.Items, refs)
			walkRefs(value.Value.AdditionalProperties.Schema, refs)
			for _, ref := range value.Value.AllOf {
				walkRefs(ref, refs)
			}
			for _, ref := range value.Value.AnyOf {
				walkRefs(ref, refs)
			}
			for _, ref := range value.Value.OneOf {
				walkRefs(ref, refs)
			}
		}
	case *openapi3.ParameterRef:
		if value == nil || value.Value == nil {
			return
		}
		walkRefs(value.Value.Schema, refs)
	}
}

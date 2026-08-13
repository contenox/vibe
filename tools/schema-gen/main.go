package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"reflect"

	"github.com/contenox/contenox/internal/kernel/taskengine"
	"github.com/contenox/contenox/internal/kernel/taskengine/llmretry"
	"github.com/contenox/contenox/internal/services/hitlservice"
	"github.com/invopop/jsonschema"
)

const (
	modulePath   = "github.com/contenox/contenox"
	schemaBaseID = "https://contenox.com/schema/"
	commentRoot  = "internal"
)

type target struct {
	file  string
	title string
	value any
}

func targets() []target {
	return []target{
		{
			// The filename carries hitlservice.PolicySchemaVersion so a version
			// bump cannot leave the published schema named for the old wire format.
			file:  fmt.Sprintf("hitl-policy-v%d.schema.json", hitlservice.PolicySchemaVersion),
			title: "contenox HITL policy",
			value: &hitlservice.Policy{},
		},
		{
			// Unversioned: taskengine declares no schema-version constant for
			// chains, and inventing one here would be the drift this tool exists
			// to prevent.
			file:  "task-chain.schema.json",
			title: "contenox task chain",
			value: &taskengine.TaskChainDefinition{},
		},
	}
}

func main() {
	var projectDir, outputDir string
	flag.StringVar(&projectDir, "project", ".", "Module root whose Go doc comments become schema descriptions.")
	flag.StringVar(&outputDir, "output", "schema", "Output directory, relative to -project unless absolute.")
	flag.Parse()

	if err := os.Chdir(projectDir); err != nil {
		log.Fatalf("chdir to project %q: %v", projectDir, err)
	}
	absOut, err := filepath.Abs(outputDir)
	if err != nil {
		log.Fatal(err)
	}
	if err := os.MkdirAll(absOut, 0o755); err != nil {
		log.Fatal(err)
	}

	r, err := newReflector()
	if err != nil {
		log.Fatal(err)
	}
	for _, t := range targets() {
		schema := r.Reflect(t.value)
		schema.ID = jsonschema.ID(schemaBaseID + t.file)
		schema.Title = t.title
		applyLoaderFixups(schema)

		path := filepath.Join(absOut, t.file)
		if err := writeSchema(path, schema); err != nil {
			log.Fatal(err)
		}
		fmt.Printf("wrote %s\n", path)
	}
}

func newReflector() (*jsonschema.Reflector, error) {
	r := &jsonschema.Reflector{
		// Requiredness is opt-in via `jsonschema:"required"` rather than derived
		// from the absence of `omitempty`: the Go tags encode wire shape, not the
		// loader's contract, and deriving it marked every optional field in every
		// chain the repo ships as missing.
		RequiredFromJSONSchemaTags: true,
		// The loaders ignore unknown keys except where they opt out explicitly
		// (see applyLoaderFixups); every hitl policy this repo ships also carries
		// `//`-prefixed comment keys that a closed schema would reject.
		AllowAdditionalProperties: true,
		ExpandedStruct:            true,
		Mapper:                    mapDuration,
	}
	if err := r.AddGoComments(modulePath, commentRoot, jsonschema.WithFullComment()); err != nil {
		return nil, fmt.Errorf("read go comments under %s: %w", commentRoot, err)
	}
	return r, nil
}

var durationType = reflect.TypeOf(llmretry.Duration(0))

// mapDuration mirrors llmretry.Duration's codec: it marshals as a duration
// string and unmarshals from a string or a nanosecond count, so the reflected
// int64 alone would reject every retry_policy this repo ships.
func mapDuration(t reflect.Type) *jsonschema.Schema {
	if t != durationType {
		return nil
	}
	return &jsonschema.Schema{
		OneOf: []*jsonschema.Schema{
			{Type: "string", Description: "Go duration string, e.g. \"10s\"."},
			{Type: "integer", Description: "Nanoseconds."},
		},
	}
}

func applyLoaderFixups(schema *jsonschema.Schema) {
	// hitlservice.loadPolicy runs rejectUnknownSubObjectFields over exactly
	// these two blocks, so they are the only closed objects in either document.
	closeObject(schema, "ComputeBounds")
	closeObject(schema, "TrustedBinaries")
	widenMaxTokens(schema)
}

func closeObject(schema *jsonschema.Schema, def string) {
	if d, ok := schema.Definitions[def]; ok {
		d.AdditionalProperties = jsonschema.FalseSchema
	}
}

// widenMaxTokens mirrors LLMExecutionConfig.unmarshalMaxTokens, which accepts
// an integer or an unexpanded string macro for a field typed *int.
func widenMaxTokens(schema *jsonschema.Schema) {
	def, ok := schema.Definitions["LLMExecutionConfig"]
	if !ok || def.Properties == nil {
		return
	}
	current, ok := def.Properties.Get("max_tokens")
	if !ok {
		return
	}
	def.Properties.Set("max_tokens", &jsonschema.Schema{
		Description: current.Description,
		OneOf: []*jsonschema.Schema{
			{Type: "integer"},
			{Type: "string", Description: "Unexpanded macro, e.g. \"{{var:max_tokens|8192}}\"."},
		},
	})
}

func writeSchema(path string, schema *jsonschema.Schema) error {
	data, err := json.MarshalIndent(schema, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal %s: %w", path, err)
	}
	return os.WriteFile(path, append(data, '\n'), 0o644)
}

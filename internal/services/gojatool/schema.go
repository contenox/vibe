package gojatool

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/contenox/contenox/internal/kernel/taskengine"
	"github.com/getkin/kin-openapi/openapi3"
)

// --- Tool schemas ------------------------------------------------------------
// Descriptions are terse by default: most limits (deadline, output cap,
// call-depth, JSON-only) are taught by their error at the moment they bite.
// The completion value, host.tool's existence and address form, and the
// absence of ambient I/O are stated up front instead, since nothing would
// ever teach those.

const evalDescription = "Run JavaScript (ES2023) in a sandbox and get its value back as JSON. " +
	"The result is the last expression evaluated — end with the value you want, or an explicit `x` on its own line; a program that ends on a statement returns null. " +
	"There is NO network, NO filesystem, NO require/import and NO async: the only way out is host.tool(\"provider.tool_name\", {args}), which runs that tool under the same approval rules as your own tool calls and returns its result. " +
	"A tool that answers with structured data arrives as an object; a tool that answers in prose arrives as {text: \"…\"} — use .text, since the wording is written for a reader and is not a format. " +
	"Use it for transforms, parsing and arithmetic over data you already have."

func evalSchema() taskengine.Tool {
	return taskengine.Tool{
		Type: "function",
		Function: taskengine.FunctionTool{
			Name:        ToolEval,
			Description: evalDescription,
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"code": map[string]any{
						"type":        "string",
						"description": "The program to run. Its last expression is the result.",
					},
					"deadline_ms": map[string]any{
						"type":        "integer",
						"description": fmt.Sprintf("Optional time budget in milliseconds (default %d, ceiling %d).", DefaultDeadline.Milliseconds(), MaxDeadline.Milliseconds()),
					},
				},
				"required": []string{"code"},
			},
		},
	}
}

// GetToolsForToolsByName returns the provider's tool list, or one tool by name.
// Script tools carry the description and schema their FILE declares: the
// operator who wrote the script owns what the model is told about it.
func (t *Toolset) GetToolsForToolsByName(_ context.Context, name string) ([]taskengine.Tool, error) {
	all := make([]taskengine.Tool, 0, len(t.scripts)+1)
	all = append(all, evalSchema())
	for _, sc := range t.scripts {
		all = append(all, taskengine.Tool{
			Type: "function",
			Function: taskengine.FunctionTool{
				Name:        sc.Name,
				Description: sc.Description,
				Parameters:  sc.Schema,
			},
		})
	}

	if name == ToolsProviderName || name == "" {
		return all, nil
	}
	for _, tool := range all {
		if tool.Function.Name == name {
			return []taskengine.Tool{tool}, nil
		}
	}
	return nil, fmt.Errorf("goja: unknown tool %s", echoArg(name))
}

// GetSchemasForSupportedTools publishes the toolset's OpenAPI 3.1 contract:
// one request/response pair per tool the provider declares — goja_eval and
// every loaded script tool alike. Request schemas are converted from the
// descriptors GetToolsForToolsByName hands the model, which for a script tool
// is the schema its own FILE declares, so the published contract cannot drift
// from what the provider accepts and no operator-owned script schema is
// paraphrased here. Components are keyed by the tool name verbatim (not a
// camel-cased form), since a script may declare any name and two names must
// never collapse onto one key.
func (t *Toolset) GetSchemasForSupportedTools(ctx context.Context) (map[string]*openapi3.T, error) {
	declared, err := t.GetToolsForToolsByName(ctx, ToolsProviderName)
	if err != nil {
		return nil, err
	}
	schemas := make(map[string]*openapi3.SchemaRef, 2*len(declared))
	for _, tool := range declared {
		req, err := schemaFromParameters(tool.Function.Parameters)
		if err != nil {
			return nil, fmt.Errorf("goja: publish schema for %s: %w", tool.Function.Name, err)
		}
		schemas[tool.Function.Name+"Request"] = req
		schemas[tool.Function.Name+"Response"] = resultSchema()
	}
	schema := &openapi3.T{
		OpenAPI: "3.1.0",
		Info: &openapi3.Info{
			Title:       "Goja Sandbox Tools",
			Description: "Run JavaScript (ES2023) in a sandbox: goja_eval for a program the model writes, plus one tool per operator-installed script. No network, no filesystem, no require/import and no async — the only way out is host.tool(\"provider.tool_name\", {args}), which runs that tool under the same approval rules as the model's own tool calls. Every tool returns the same result envelope.",
			Version:     "1.0.0",
		},
		Paths: openapi3.NewPaths(),
		Components: &openapi3.Components{
			Schemas: schemas,
		},
	}
	return map[string]*openapi3.T{ToolsProviderName: schema}, nil
}

// schemaFromParameters converts a tool descriptor's JSON Schema parameters
// into an OpenAPI schema. The descriptor stays the single source of truth: the
// published contract is a rendering of it, never a second copy that could
// disagree.
func schemaFromParameters(params any) (*openapi3.SchemaRef, error) {
	raw, err := json.Marshal(params)
	if err != nil {
		return nil, fmt.Errorf("encode parameters: %w", err)
	}
	var s openapi3.Schema
	if err := s.UnmarshalJSON(raw); err != nil {
		return nil, fmt.Errorf("parameters are not a JSON Schema object: %w", err)
	}
	return &openapi3.SchemaRef{Value: &s}, nil
}

// resultSchema declares the Result envelope every goja tool returns as
// DataTypeJSON — goja_eval and script tools alike. A refused or failed
// execution returns an error instead, so nothing here is a failure marker.
func resultSchema() *openapi3.SchemaRef {
	return &openapi3.SchemaRef{Value: &openapi3.Schema{
		Type: &openapi3.Types{openapi3.TypeObject},
		Properties: map[string]*openapi3.SchemaRef{
			"value": {Value: &openapi3.Schema{
				Description: "The script's return value as JSON — for goja_eval, the last expression evaluated; a program that ends on a statement yields null. On truncation it becomes a JSON string holding the head of the original plus the notice.",
			}},
			"truncated": {Value: &openapi3.Schema{
				Type:        &openapi3.Types{openapi3.TypeBoolean},
				Description: "True when value hit the output cap. Absent means the value is complete.",
			}},
			"notice": {Value: &openapi3.Schema{
				Type:        &openapi3.Types{openapi3.TypeString},
				Description: "The truncation marker, naming the cap and the remedy. Absent when nothing was cut.",
			}},
			"logs": {Value: &openapi3.Schema{
				Type:        &openapi3.Types{openapi3.TypeArray},
				Items:       &openapi3.SchemaRef{Value: &openapi3.Schema{Type: &openapi3.Types{openapi3.TypeString}, Description: "One console line."}},
				Description: "The script's console.* lines, in order. Absent when it logged nothing.",
			}},
			"logs_truncated": {Value: &openapi3.Schema{
				Type:        &openapi3.Types{openapi3.TypeBoolean},
				Description: "True when console output hit its own cap.",
			}},
			"duration_ms": {Value: &openapi3.Schema{
				Type:        &openapi3.Types{openapi3.TypeInteger},
				Description: "Wall time spent inside the sandbox in milliseconds, including any host.tool calls the script made.",
			}},
		},
		Required: []string{"value", "duration_ms"},
	}}
}

package jqtool

// Tool schema. Kept terse: the deadline, caps, containment boundary and
// exactly-one-source rule all have errors that state them precisely when they
// bite, so they aren't spelled out here. The description does state the
// things no error would ever teach: that the tool never writes, that it isn't
// goja_eval, that its input is one file or one string, and that YAML is
// accepted — each a case where a model would otherwise succeed at the wrong
// thing rather than get a correcting error.
//
// The argument set is declared once (queryProperties) and rendered twice: into
// the model-facing descriptor and into the published OpenAPI contract, so the
// two cannot drift.

import (
	"context"
	"fmt"

	"github.com/contenox/contenox/internal/kernel/taskengine"
	"github.com/getkin/kin-openapi/openapi3"
)

const queryDescription = "Run a jq program over ONE JSON or YAML document and get the values it emits. " +
	"Point it at a workspace file with `path`, or pass the document itself in `input` — as a JSON/YAML string, or as an already-decoded JSON value — exactly one of the two. " +
	"Use it instead of reading a large config file: `.tasks[]|select(.handler==\"tools\")|.id` costs a few tokens where the file costs thousands. " +
	"It NEVER writes: a filter like `.a = 1` returns a modified COPY as its result and the file on disk is untouched. " +
	"It is not goja_eval — reach for jq for declarative shape-work over one document (select, project, map, group_by, keys, length), and for goja_eval when you need imperative logic or to call another tool. " +
	"The input is one file or one document, not a pipe: never paste large tool output into `input`."

// queryProperty is one jq_query argument, declared once and rendered into both
// the descriptor and the OpenAPI components.
type queryProperty struct {
	name string
	// types is the JSON Schema type set: one entry for an ordinary argument,
	// several when Exec accepts a union (see input, which resolveInput takes
	// either as a document string or as an already-decoded JSON value).
	types []string
	// itemType is the element type of the array branch, required whenever
	// types includes "array": an OpenAPI 3.1 document whose array declares no
	// items is not a valid document. Empty means "any JSON value".
	itemType    string
	description string
	// enum, when set, is the closed value set — declared, never left to prose.
	enum     []string
	required bool
}

// jsonType renders the type as the bare string a single type spells and the
// list a union spells — the two shapes a JSON Schema "type" takes.
func (p queryProperty) jsonType() any {
	if len(p.types) == 1 {
		return p.types[0]
	}
	out := make([]any, 0, len(p.types))
	for _, t := range p.types {
		out = append(out, t)
	}
	return out
}

// hasArray reports whether the type set carries an array branch, which must
// declare items in both renderings.
func (p queryProperty) hasArray() bool {
	for _, t := range p.types {
		if t == openapi3.TypeArray {
			return true
		}
	}
	return false
}

// queryProperties is the single source of truth for jq_query's arguments.
func queryProperties() []queryProperty {
	return []queryProperty{
		{
			name:        "filter",
			types:       []string{"string"},
			description: "The jq program, e.g. \".tasks[] | select(.handler==\\\"tools\\\") | .id\". Use \".\" for the whole document, \"keys\" for an object's fields, \"length\" to count.",
			required:    true,
		},
		{
			name:        "path",
			types:       []string{"string"},
			description: "Document to query, relative to the workspace root. Mutually exclusive with input.",
		},
		{
			// The union is the one resolveInput really accepts: a string is
			// parsed as a JSON or YAML document, any other JSON value is taken
			// as the already-decoded document it is. "null" is left out
			// deliberately — a null input reads as no input at all, and the
			// call is refused for having no source.
			name:        "input",
			types:       []string{"string", "object", "array", "number", "integer", "boolean"},
			description: "The document itself: a JSON or YAML string, or an already-decoded JSON value (object, array, number, boolean). Mutually exclusive with path. For small documents you already have — not for large pasted output.",
		},
		{
			name:        "format",
			types:       []string{"string"},
			enum:        []string{FormatJSON, FormatYAML},
			description: "Parser to use. Default: from the file extension, then from the content.",
		},
		{
			name:        "max",
			types:       []string{"integer"},
			description: fmt.Sprintf("Maximum values to return (default %d, ceiling %d). The result says when it was capped. A decimal string is read as the number it spells.", defaultMaxResults, maxResultsCeiling),
		},
		{
			name:        "deadline_ms",
			types:       []string{"integer"},
			description: fmt.Sprintf("Time budget in milliseconds (default %d, ceiling %d). A decimal string is read as the number it spells.", DefaultDeadline.Milliseconds(), MaxDeadline.Milliseconds()),
		},
	}
}

// queryRequired renders the table's required set, in table order.
func queryRequired() []string {
	var out []string
	for _, p := range queryProperties() {
		if p.required {
			out = append(out, p.name)
		}
	}
	return out
}

// queryToolParameters renders the table as the descriptor's JSON Schema — what
// actually reaches the provider.
func queryToolParameters() map[string]any {
	props := make(map[string]any, len(queryProperties()))
	for _, p := range queryProperties() {
		prop := map[string]any{"type": p.jsonType(), "description": p.description}
		if len(p.enum) > 0 {
			prop["enum"] = append([]string(nil), p.enum...)
		}
		if p.hasArray() {
			items := map[string]any{}
			if p.itemType != "" {
				items["type"] = p.itemType
			}
			prop["items"] = items
		}
		props[p.name] = prop
	}
	return map[string]any{
		"type":       "object",
		"properties": props,
		"required":   queryRequired(),
	}
}

// queryRequestSchemaProperties renders the same table as OpenAPI schema refs.
func queryRequestSchemaProperties() map[string]*openapi3.SchemaRef {
	out := make(map[string]*openapi3.SchemaRef, len(queryProperties()))
	for _, p := range queryProperties() {
		types := openapi3.Types(append([]string(nil), p.types...))
		s := &openapi3.Schema{Type: &types, Description: p.description}
		for _, v := range p.enum {
			s.Enum = append(s.Enum, v)
		}
		if p.hasArray() {
			items := &openapi3.Schema{}
			if p.itemType != "" {
				items.Type = &openapi3.Types{p.itemType}
			}
			s.Items = &openapi3.SchemaRef{Value: items}
		}
		out[p.name] = &openapi3.SchemaRef{Value: s}
	}
	return out
}

// queryResponseSchema declares what a successful jq_query actually returns
// (Result, as DataTypeJSON). A refused or failed call returns an error rather
// than this payload, so nothing here is optional-on-failure.
func queryResponseSchema() *openapi3.SchemaRef {
	return &openapi3.SchemaRef{Value: &openapi3.Schema{
		Type: &openapi3.Types{openapi3.TypeObject},
		Properties: map[string]*openapi3.SchemaRef{
			"filter": {Value: &openapi3.Schema{
				Type:        &openapi3.Types{openapi3.TypeString},
				Description: "The jq program that ran, echoed back clamped.",
			}},
			"source": {Value: &openapi3.Schema{
				Type:        &openapi3.Types{openapi3.TypeString},
				Description: "What was queried: the workspace-relative path, or \"" + inlineSource + "\". Never a host-absolute path.",
			}},
			"format": {Value: &openapi3.Schema{
				Type:        &openapi3.Types{openapi3.TypeString},
				Enum:        []any{FormatJSON, FormatYAML},
				Description: "The parser actually used; it may have been sniffed from the extension or the content rather than declared.",
			}},
			"documents": {Value: &openapi3.Schema{
				Type:        &openapi3.Types{openapi3.TypeInteger},
				Description: "How many documents the input carried: 1 for an ordinary file, more for a JSON Lines or multi-document YAML stream.",
			}},
			"values": {Value: &openapi3.Schema{
				Type:        &openapi3.Types{openapi3.TypeArray},
				Items:       &openapi3.SchemaRef{Value: &openapi3.Schema{Description: "One emitted value — any JSON value the filter produced."}},
				Description: "The values the filter emitted, in emission order.",
			}},
			"count": {Value: &openapi3.Schema{
				Type:        &openapi3.Types{openapi3.TypeInteger},
				Description: "How many values are in values.",
			}},
			"truncated": {Value: &openapi3.Schema{
				Type:        &openapi3.Types{openapi3.TypeBoolean},
				Description: "True when a cap stopped the result before the filter was done; never set without note. Absent means nothing was cut.",
			}},
			"note": {Value: &openapi3.Schema{
				Type:        &openapi3.Types{openapi3.TypeString},
				Description: "Why the result is not the whole answer — which cap bit and what to raise. Absent when there is nothing to say.",
			}},
		},
		Required: []string{"filter", "source", "format", "documents", "values", "count"},
	}}
}

// queryRequestSchema is the declared request contract for jq_query.
func queryRequestSchema() *openapi3.SchemaRef {
	return &openapi3.SchemaRef{Value: &openapi3.Schema{
		Type:       &openapi3.Types{openapi3.TypeObject},
		Properties: queryRequestSchemaProperties(),
		Required:   queryRequired(),
	}}
}

// GetSchemasForSupportedTools publishes the toolset's OpenAPI 3.1 contract:
// jq_query's request and response. The request schema is rendered from the
// same property table the descriptor renders (queryProperties), so the
// declared contract and what the model receives cannot drift.
func (h *tools) GetSchemasForSupportedTools(context.Context) (map[string]*openapi3.T, error) {
	schema := &openapi3.T{
		OpenAPI: "3.1.0",
		Info: &openapi3.Info{
			Title:       "JQ Tools",
			Description: "Query ONE JSON or YAML document with a jq program and get the values it emits. Read-only: a filter that looks like an assignment returns a modified copy and never touches the file.",
			Version:     "1.0.0",
		},
		Paths: openapi3.NewPaths(),
		Components: &openapi3.Components{
			Schemas: map[string]*openapi3.SchemaRef{
				"JqQueryRequest":  queryRequestSchema(),
				"JqQueryResponse": queryResponseSchema(),
			},
		},
	}
	return map[string]*openapi3.T{ToolsProviderName: schema}, nil
}

func queryTool() taskengine.Tool {
	return taskengine.Tool{
		Type: "function",
		Function: taskengine.FunctionTool{
			Name:        ToolQuery,
			Description: queryDescription,
			Parameters:  queryToolParameters(),
		},
	}
}

func (h *tools) GetToolsForToolsByName(_ context.Context, name string) ([]taskengine.Tool, error) {
	all := []taskengine.Tool{queryTool()}
	if name == ToolsProviderName || name == "" {
		return all, nil
	}
	for _, t := range all {
		if t.Function.Name == name {
			return []taskengine.Tool{t}, nil
		}
	}
	return nil, fmt.Errorf("jq: unknown tool %s", echoArg(name))
}

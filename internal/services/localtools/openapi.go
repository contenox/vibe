package localtools

// The published OpenAPI contract for this package's toolsets. Request schemas
// are CONVERTED from the very descriptors GetToolsForToolsByName hands the
// model (schemaFromParameters) rather than restated in a property table, for
// three reasons that hold across every provider here:
//
//   - local_fs and git already declare their arguments once, through the
//     shared fsTool/fsProp builders; a table beside them would be a second
//     copy of a declaration that is already single.
//   - local_fs's descriptors are CONTEXT-DEPENDENT (_verbose_tool_descriptions
//     picks a different description per call), so a hand-written table could
//     not track them at all.
//   - webtools' arguments carry shapes a flat {name,type,description} table
//     cannot hold: headers is an object with additionalProperties, body is a
//     type union.
//
// Response schemas are written from what each Exec actually returns — the
// typed results (FsWriteResult, GitStatusResult, …), the plain strings, and
// the soft-refusal and no-match payloads that are returned as RESULTS rather
// than errors and are therefore part of the contract.

import (
	"encoding/json"
	"fmt"

	"github.com/contenox/contenox/internal/kernel/taskengine"
	"github.com/getkin/kin-openapi/openapi3"
)

// toolSchemaSpec binds one declared tool to its OpenAPI component prefix and
// the response schema describing what Exec returns for it. The request schema
// is not here: it is converted from the descriptor.
type toolSchemaSpec struct {
	tool      string
	component string
	response  func() *openapi3.SchemaRef
}

// buildToolsetDoc renders one provider's OpenAPI 3.1 document: a request and a
// response component per declared tool. declared must be the descriptors the
// provider hands the model, so the published request contract is a rendering
// of them and never a second copy that could disagree. A declared tool with no
// spec, or a spec naming no declared tool, is an error rather than a document
// that silently covers less than the toolset does.
func buildToolsetDoc(provider, title, description string, declared []taskengine.Tool, specs []toolSchemaSpec) (*openapi3.T, error) {
	byName := make(map[string]taskengine.Tool, len(declared))
	for _, t := range declared {
		byName[t.Function.Name] = t
	}
	specced := make(map[string]struct{}, len(specs))
	for _, spec := range specs {
		specced[spec.tool] = struct{}{}
	}
	for _, t := range declared {
		if _, ok := specced[t.Function.Name]; !ok {
			return nil, fmt.Errorf("%s: %s is declared to the model but publishes no schema", provider, t.Function.Name)
		}
	}
	schemas := make(map[string]*openapi3.SchemaRef, 2*len(specs))
	for _, spec := range specs {
		tool, ok := byName[spec.tool]
		if !ok {
			return nil, fmt.Errorf("%s: %s declares a schema but no descriptor", provider, spec.tool)
		}
		req, err := schemaFromParameters(tool.Function.Parameters)
		if err != nil {
			return nil, fmt.Errorf("%s: publish schema for %s: %w", provider, spec.tool, err)
		}
		schemas[spec.component+"Request"] = req
		schemas[spec.component+"Response"] = spec.response()
	}
	return &openapi3.T{
		OpenAPI: "3.1.0",
		Info: &openapi3.Info{
			Title:       title,
			Description: description,
			Version:     "1.0.0",
		},
		Paths:      openapi3.NewPaths(),
		Components: &openapi3.Components{Schemas: schemas},
	}, nil
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
	renderForOpenAPI(&s)
	return &openapi3.SchemaRef{Value: &s}, nil
}

// renderForOpenAPI rewrites the two JSON Schema spellings a tool descriptor is
// allowed to use but an OpenAPI document validator rejects, without changing
// what either one means:
//
//   - a "null" member of a type union becomes nullable, since the validator
//     knows no "null" type;
//   - an array with no declared items gets the empty (any-item) schema, which
//     is what an undeclared item type already means.
//
// Applied to the converted copy only — the descriptor the model receives is
// untouched.
func renderForOpenAPI(s *openapi3.Schema) {
	if s == nil {
		return
	}
	if s.Type != nil && s.Type.Includes("null") {
		var kept openapi3.Types
		for _, t := range *s.Type {
			if t != "null" {
				kept = append(kept, t)
			}
		}
		s.Nullable = true
		if len(kept) == 0 {
			s.Type = nil
		} else {
			s.Type = &kept
		}
	}
	if s.Type != nil && s.Type.Includes(openapi3.TypeArray) && s.Items == nil {
		s.Items = &openapi3.SchemaRef{Value: &openapi3.Schema{}}
	}
	for _, p := range s.Properties {
		if p != nil {
			renderForOpenAPI(p.Value)
		}
	}
	if s.Items != nil {
		renderForOpenAPI(s.Items.Value)
	}
	if s.AdditionalProperties.Schema != nil {
		renderForOpenAPI(s.AdditionalProperties.Schema.Value)
	}
	for _, group := range [][]*openapi3.SchemaRef{s.OneOf, s.AnyOf, s.AllOf} {
		for _, r := range group {
			if r != nil {
				renderForOpenAPI(r.Value)
			}
		}
	}
}

// --- Response schema builders -------------------------------------------------

// strSchema is a described string.
func strSchema(description string) *openapi3.SchemaRef {
	return &openapi3.SchemaRef{Value: &openapi3.Schema{
		Type:        &openapi3.Types{openapi3.TypeString},
		Description: description,
	}}
}

// intSchema is a described integer.
func intSchema(description string) *openapi3.SchemaRef {
	return &openapi3.SchemaRef{Value: &openapi3.Schema{
		Type:        &openapi3.Types{openapi3.TypeInteger},
		Description: description,
	}}
}

// boolSchema is a described boolean.
func boolSchema(description string) *openapi3.SchemaRef {
	return &openapi3.SchemaRef{Value: &openapi3.Schema{
		Type:        &openapi3.Types{openapi3.TypeBoolean},
		Description: description,
	}}
}

// arraySchema is a described array of items.
func arraySchema(description string, items *openapi3.SchemaRef) *openapi3.SchemaRef {
	return &openapi3.SchemaRef{Value: &openapi3.Schema{
		Type:        &openapi3.Types{openapi3.TypeArray},
		Items:       items,
		Description: description,
	}}
}

// stringMapSchema is a described object with no fixed keys and string values —
// a header set, or anything else keyed by a name the schema cannot enumerate.
func stringMapSchema(description, value string) *openapi3.SchemaRef {
	return &openapi3.SchemaRef{Value: &openapi3.Schema{
		Type:                 &openapi3.Types{openapi3.TypeObject},
		Description:          description,
		AdditionalProperties: openapi3.AdditionalProperties{Schema: strSchema(value)},
	}}
}

// objectSchema assembles one described object.
func objectSchema(description string, props map[string]*openapi3.SchemaRef, required ...string) *openapi3.SchemaRef {
	return &openapi3.SchemaRef{Value: &openapi3.Schema{
		Type:        &openapi3.Types{openapi3.TypeObject},
		Description: description,
		Properties:  props,
		Required:    required,
	}}
}

// oneOfSchema is a result that comes back in exactly one of several disjoint
// shapes — a typed success payload, a soft refusal, a plain message.
func oneOfSchema(description string, variants ...*openapi3.SchemaRef) *openapi3.SchemaRef {
	return &openapi3.SchemaRef{Value: &openapi3.Schema{
		Description: description,
		OneOf:       variants,
	}}
}

// anyOfSchema is a result whose shapes are not disjoint, so a payload may match
// more than one variant.
func anyOfSchema(description string, variants ...*openapi3.SchemaRef) *openapi3.SchemaRef {
	return &openapi3.SchemaRef{Value: &openapi3.Schema{
		Description: description,
		AnyOf:       variants,
	}}
}

// refusalSchema is FsRefusalResult, the soft denial the mutating local_fs tools
// return as a RESULT rather than an error. It reaches the model as its Reason
// text.
func refusalSchema() *openapi3.SchemaRef {
	return objectSchema(
		"A refusal: the file was NOT written. Returned as a tool result, not an error, and reaches the model as the reason text alone.",
		map[string]*openapi3.SchemaRef{
			"refused": boolSchema("Always true on this shape."),
			"reason":  strSchema("Why the write was refused and what to do about it — the read-before-write denial, or the stale-read denial when the file changed after the read that authorized this call."),
		}, "refused", "reason")
}

// chatHistorySchema is the taskengine chat history echo and print return when
// the task input is a history rather than an argument map: the same history,
// with one message appended.
func chatHistorySchema(description string) *openapi3.SchemaRef {
	return objectSchema(description, map[string]*openapi3.SchemaRef{
		"messages": arraySchema(
			"The conversation, carried through unchanged apart from the appended message.",
			objectSchema("One message.", map[string]*openapi3.SchemaRef{
				"role":      strSchema("Who the message is from."),
				"content":   strSchema("The message text."),
				"timestamp": strSchema("When the message was created."),
			}, "role", "timestamp")),
	}, "messages")
}

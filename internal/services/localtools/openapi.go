package localtools

import (
	"encoding/json"
	"fmt"

	"github.com/contenox/contenox/internal/kernel/taskengine"
	"github.com/getkin/kin-openapi/openapi3"
)

type toolSchemaSpec struct {
	tool      string
	component string
	response  func() *openapi3.SchemaRef
}

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

func strSchema(description string) *openapi3.SchemaRef {
	return &openapi3.SchemaRef{Value: &openapi3.Schema{
		Type:        &openapi3.Types{openapi3.TypeString},
		Description: description,
	}}
}

func intSchema(description string) *openapi3.SchemaRef {
	return &openapi3.SchemaRef{Value: &openapi3.Schema{
		Type:        &openapi3.Types{openapi3.TypeInteger},
		Description: description,
	}}
}

func boolSchema(description string) *openapi3.SchemaRef {
	return &openapi3.SchemaRef{Value: &openapi3.Schema{
		Type:        &openapi3.Types{openapi3.TypeBoolean},
		Description: description,
	}}
}

func arraySchema(description string, items *openapi3.SchemaRef) *openapi3.SchemaRef {
	return &openapi3.SchemaRef{Value: &openapi3.Schema{
		Type:        &openapi3.Types{openapi3.TypeArray},
		Items:       items,
		Description: description,
	}}
}

func stringMapSchema(description, value string) *openapi3.SchemaRef {
	return &openapi3.SchemaRef{Value: &openapi3.Schema{
		Type:                 &openapi3.Types{openapi3.TypeObject},
		Description:          description,
		AdditionalProperties: openapi3.AdditionalProperties{Schema: strSchema(value)},
	}}
}

func objectSchema(description string, props map[string]*openapi3.SchemaRef, required ...string) *openapi3.SchemaRef {
	return &openapi3.SchemaRef{Value: &openapi3.Schema{
		Type:        &openapi3.Types{openapi3.TypeObject},
		Description: description,
		Properties:  props,
		Required:    required,
	}}
}

func oneOfSchema(description string, variants ...*openapi3.SchemaRef) *openapi3.SchemaRef {
	return &openapi3.SchemaRef{Value: &openapi3.Schema{
		Description: description,
		OneOf:       variants,
	}}
}

func anyOfSchema(description string, variants ...*openapi3.SchemaRef) *openapi3.SchemaRef {
	return &openapi3.SchemaRef{Value: &openapi3.Schema{
		Description: description,
		AnyOf:       variants,
	}}
}

func refusalSchema() *openapi3.SchemaRef {
	return objectSchema(
		"A refusal: the file was NOT written. Returned as a tool result, not an error, and reaches the model as the reason text alone.",
		map[string]*openapi3.SchemaRef{
			"refused": boolSchema("Always true on this shape."),
			"reason":  strSchema("Why the write was refused and what to do about it — the read-before-write denial, or the stale-read denial when the file changed after the read that authorized this call."),
		}, "refused", "reason")
}

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

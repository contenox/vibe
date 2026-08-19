package echotool

import (
	"context"
	"fmt"

	"github.com/contenox/contenox/internal/kernel/taskengine"
	"github.com/getkin/kin-openapi/openapi3"
)

// Terse by default: a description is paid on every turn this toolset is
// declared. Two things are stated anyway since nothing later re-teaches them —
// that the tool has no effect at all, so it is never the answer to a real task,
// and that a chain step over a conversation answers with the conversation.
const echoToolDoc = "Returns its input unchanged — a wiring and test fixture, NOT a capability. " +
	"It reads no file, makes no network call and starts no process, so a call changes nothing and tells you nothing you did not already pass in. " +
	"Called with `input` it echoes that argument; run as a chain step over a conversation it appends one assistant message quoting the last user message. " +
	"Use it to prove a tool call reaches this agent at all — anything you actually want done needs a different toolset."

// echoProperty is one echo argument: name, JSON Schema type, the description the
// model reads, and whether the tool refuses without it.
type echoProperty struct {
	name        string
	typ         string
	description string
	required    bool
}

// echoProperties is the single source of truth for echo's arguments. lim is the
// effective policy, so what the model is told the cap is and what a call
// actually enforces cannot disagree.
func echoProperties(lim limit) []echoProperty {
	return []echoProperty{
		{
			name: "input",
			typ:  "string",
			description: fmt.Sprintf(
				"The text to echo back, returned verbatim; a non-string value is rendered the way the engine renders it. Capped at %d bytes — a longer value comes back cut at a character boundary, with a trailing count of what was withheld.",
				lim.maxBytes),
			required: true,
		},
	}
}

func echoRequired(lim limit) []string {
	var out []string
	for _, p := range echoProperties(lim) {
		if p.required {
			out = append(out, p.name)
		}
	}
	return out
}

// echoToolParameters renders the table as the descriptor's JSON Schema — what
// actually reaches the provider.
func echoToolParameters(lim limit) map[string]any {
	props := make(map[string]any, len(echoProperties(lim)))
	for _, p := range echoProperties(lim) {
		props[p.name] = map[string]any{"type": p.typ, "description": p.description}
	}
	return map[string]any{
		"type":       "object",
		"properties": props,
		"required":   echoRequired(lim),
	}
}

// echoRequestSchema renders the same table as the published OpenAPI request schema.
func echoRequestSchema(lim limit) *openapi3.SchemaRef {
	props := make(map[string]*openapi3.SchemaRef, len(echoProperties(lim)))
	for _, p := range echoProperties(lim) {
		props[p.name] = &openapi3.SchemaRef{Value: &openapi3.Schema{
			Type:        &openapi3.Types{p.typ},
			Description: p.description,
		}}
	}
	return &openapi3.SchemaRef{Value: &openapi3.Schema{
		Type:       &openapi3.Types{openapi3.TypeObject},
		Properties: props,
		Required:   echoRequired(lim),
	}}
}

func (h *tools) GetToolsForToolsByName(ctx context.Context, name string) ([]taskengine.Tool, error) {
	lim := limitFrom(ctx, h.name)
	allTools := []taskengine.Tool{
		{
			Type: "function",
			Function: taskengine.FunctionTool{
				Name:        ToolEcho,
				Description: echoToolDoc,
				Parameters:  echoToolParameters(lim),
			},
		},
	}

	if name == h.name || name == "" {
		return allTools, nil
	}
	for _, t := range allTools {
		if t.Function.Name == name {
			return []taskengine.Tool{t}, nil
		}
	}
	return nil, fmt.Errorf("echo: unknown tools: %s", echoName(name))
}

// GetSchemasForSupportedTools publishes the toolset's OpenAPI 3.1 contract under
// the registered key. The request schema is rendered from the same property
// table the descriptor renders, so the declared contract and what the model
// receives cannot drift.
func (h *tools) GetSchemasForSupportedTools(ctx context.Context) (map[string]*openapi3.T, error) {
	lim := limitFrom(ctx, h.name)
	doc := &openapi3.T{
		OpenAPI: "3.1.0",
		Info: &openapi3.Info{
			Title:       "Echo Tools",
			Description: "Returns its input unchanged. A test and wiring fixture — it reads nothing, writes nothing and spawns nothing, so the only thing a call proves is that the call arrived.",
			Version:     "1.0.0",
		},
		Paths: openapi3.NewPaths(),
		Components: &openapi3.Components{
			Schemas: map[string]*openapi3.SchemaRef{
				"EchoRequest":  echoRequestSchema(lim),
				"EchoResponse": echoResponseSchema(),
			},
		},
	}
	return map[string]*openapi3.T{h.name: doc}, nil
}

// echoResponseSchema follows the shape of the input, which is why it is a oneOf:
// a `tools` task accepts any input type and echo passes the type through.
func echoResponseSchema() *openapi3.SchemaRef {
	return &openapi3.SchemaRef{Value: &openapi3.Schema{
		Description: "What echo returns, following the shape of its input.",
		OneOf: []*openapi3.SchemaRef{
			echoString("The input echoed back: the input argument as given, a non-string argument rendered by the engine, or \"nothing to echo\" when the call carries no input at all."),
			echoChatHistorySchema("The conversation with one assistant message appended, \"Echo: \" followed by the last user message — or \"Echo: nothing to echo\" when it holds none."),
		},
	}}
}

func echoChatHistorySchema(description string) *openapi3.SchemaRef {
	return &openapi3.SchemaRef{Value: &openapi3.Schema{
		Type:        &openapi3.Types{openapi3.TypeObject},
		Description: description,
		Properties: map[string]*openapi3.SchemaRef{
			"messages": {Value: &openapi3.Schema{
				Type:        &openapi3.Types{openapi3.TypeArray},
				Description: "The conversation, carried through unchanged apart from the appended message.",
				Items: &openapi3.SchemaRef{Value: &openapi3.Schema{
					Type:        &openapi3.Types{openapi3.TypeObject},
					Description: "One message.",
					Properties: map[string]*openapi3.SchemaRef{
						"role":      echoString("Who the message is from."),
						"content":   echoString("The message text."),
						"timestamp": echoString("When the message was created."),
					},
					Required: []string{"role", "timestamp"},
				}},
			}},
		},
		Required: []string{"messages"},
	}}
}

func echoString(description string) *openapi3.SchemaRef {
	return &openapi3.SchemaRef{Value: &openapi3.Schema{
		Type:        &openapi3.Types{openapi3.TypeString},
		Description: description,
	}}
}

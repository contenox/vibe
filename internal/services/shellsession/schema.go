package shellsession

import (
	"context"
	"fmt"

	"github.com/contenox/contenox/internal/kernel/taskengine"
	"github.com/getkin/kin-openapi/openapi3"
)

const runToolDoc = "Submit ONE command line to this chat's persistent shell (a real terminal rooted at the session workspace). " +
	"The shell keeps its working directory, environment, and history between calls, and long-running processes stay alive — a second run while one is still going types into the same running shell (that is normal shell stdin behavior, not an error). " +
	"Returns quickly with {offset, output} where output is a short initial snapshot, NOT the full result: it does not block until the command finishes. " +
	"To follow a command's ongoing output, call " + ToolRead + " with 'since' set to the returned offset. Requires approval under the active HITL policy, one approval per line."

const readToolDoc = "Read scrollback from this chat's persistent shell. Terminal output is never streamed into your context automatically — read it here when you need it. " +
	"Pass 'since' (an offset from a previous " + ToolRun + "/" + ToolRead + " result) to get only new output since that marker, or 'tail_bytes' to get the last N bytes. With neither, returns the full retained scrollback. " +
	"Returns {content, from_offset, next_offset}; use next_offset as the next 'since'. This read is not gated by HITL."

type shellProperty struct {
	name        string
	typ         string
	description string
	required    bool
}

type shellToolSpec struct {
	name        string
	component   string
	description string
	props       []shellProperty
	response    func() *openapi3.SchemaRef
}

func shellToolSpecs() []shellToolSpec {
	return []shellToolSpec{
		{
			name:        ToolRun,
			component:   "ShellSessionRun",
			description: runToolDoc,
			props: []shellProperty{{
				name:        "command",
				typ:         "string",
				description: "The single command line to run in the shell, e.g. \"go test ./... 2>&1 | tail -n 40\".",
				required:    true,
			}},
			response: runResponseSchema,
		},
		{
			name:        ToolRead,
			component:   "ShellSessionRead",
			description: readToolDoc,
			props: []shellProperty{
				{
					name:        "since",
					typ:         "integer",
					description: "Return output at/after this scrollback offset (from a prior result's offset/next_offset).",
				},
				{
					name:        "tail_bytes",
					typ:         "integer",
					description: "When 'since' is omitted, return only the last N bytes of scrollback.",
				},
			},
			response: readResponseSchema,
		},
	}
}

func (s shellToolSpec) required() []string {
	var out []string
	for _, p := range s.props {
		if p.required {
			out = append(out, p.name)
		}
	}
	return out
}

func (s shellToolSpec) parameters() map[string]any {
	props := make(map[string]any, len(s.props))
	for _, p := range s.props {
		props[p.name] = map[string]any{"type": p.typ, "description": p.description}
	}
	params := map[string]any{
		"type":       "object",
		"properties": props,
	}
	if required := s.required(); len(required) > 0 {
		params["required"] = required
	}
	return params
}

func (s shellToolSpec) requestSchema() *openapi3.SchemaRef {
	props := make(map[string]*openapi3.SchemaRef, len(s.props))
	for _, p := range s.props {
		props[p.name] = &openapi3.SchemaRef{Value: &openapi3.Schema{
			Type:        &openapi3.Types{p.typ},
			Description: p.description,
		}}
	}
	return &openapi3.SchemaRef{Value: &openapi3.Schema{
		Type:       &openapi3.Types{openapi3.TypeObject},
		Properties: props,
		Required:   s.required(),
	}}
}

func (s shellToolSpec) tool() taskengine.Tool {
	return taskengine.Tool{
		Type: "function",
		Function: taskengine.FunctionTool{
			Name:        s.name,
			Description: s.description,
			Parameters:  s.parameters(),
		},
	}
}

// GetSchemasForSupportedTools publishes one OpenAPI 3.1 request/response pair per declared tool, rendered from the same table as the descriptors (shellToolSpecs).
func (h *tools) GetSchemasForSupportedTools(context.Context) (map[string]*openapi3.T, error) {
	specs := shellToolSpecs()
	schemas := make(map[string]*openapi3.SchemaRef, 2*len(specs))
	for _, spec := range specs {
		schemas[spec.component+"Request"] = spec.requestSchema()
		schemas[spec.component+"Response"] = spec.response()
	}
	schema := &openapi3.T{
		OpenAPI: "3.1.0",
		Info: &openapi3.Info{
			Title:       "Shell Session Tools",
			Description: "One persistent terminal per chat session, rooted at the session workspace: it keeps its working directory, environment and history between calls, and long-running processes survive across turns. Running a line is submission, not completion — it returns an initial snapshot and an offset, and the output is followed by reading scrollback from that offset. Terminal output never enters the context on its own.",
			Version:     "1.0.0",
		},
		Paths: openapi3.NewPaths(),
		Components: &openapi3.Components{
			Schemas: schemas,
		},
	}
	return map[string]*openapi3.T{ToolsProviderName: schema}, nil
}

func runResponseSchema() *openapi3.SchemaRef {
	return &openapi3.SchemaRef{Value: &openapi3.Schema{
		Type:        &openapi3.Types{openapi3.TypeObject},
		Description: "The submission receipt. The command may still be running when this returns.",
		Properties: map[string]*openapi3.SchemaRef{
			"offset": {Value: &openapi3.Schema{
				Type:        &openapi3.Types{openapi3.TypeInteger},
				Description: "Scrollback offset the snapshot ends at — pass it as " + ToolRead + "'s 'since' to follow what comes next.",
			}},
			"output": {Value: &openapi3.Schema{
				Type:        &openapi3.Types{openapi3.TypeString},
				Description: "A short initial snapshot of terminal output, NOT the full result: the call does not block until the command finishes.",
			}},
			"started_new_shell": {Value: &openapi3.Schema{
				Type:        &openapi3.Types{openapi3.TypeBoolean},
				Description: "True when this call created the session's shell rather than typing into an existing one. Absent when the shell was already running.",
			}},
			"note": {Value: &openapi3.Schema{
				Type:        &openapi3.Types{openapi3.TypeString},
				Description: "Present only when the snapshot is empty: says the command may still be running and names the offset to poll from. Absent when there is output.",
			}},
		},
		Required: []string{"offset", "output"},
	}}
}

func readResponseSchema() *openapi3.SchemaRef {
	return &openapi3.SchemaRef{Value: &openapi3.Schema{
		Type:        &openapi3.Types{openapi3.TypeObject},
		Description: "A slice of the session's retained scrollback.",
		Properties: map[string]*openapi3.SchemaRef{
			"content": {Value: &openapi3.Schema{
				Type:        &openapi3.Types{openapi3.TypeString},
				Description: "The scrollback read: everything since 'since', the last 'tail_bytes' bytes, or the whole retained buffer when neither was given. Empty when nothing new has been produced.",
			}},
			"from_offset": {Value: &openapi3.Schema{
				Type:        &openapi3.Types{openapi3.TypeInteger},
				Description: "The offset this content starts at, which is past the requested one when the buffer had already discarded older output.",
			}},
			"next_offset": {Value: &openapi3.Schema{
				Type:        &openapi3.Types{openapi3.TypeInteger},
				Description: "The offset this content ends at — pass it as the next 'since'.",
			}},
			"exists": {Value: &openapi3.Schema{
				Type:        &openapi3.Types{openapi3.TypeBoolean},
				Description: "Whether a shell exists for this session at all. False is not a failure: the shell is created on the first " + ToolRun + ".",
			}},
			"note": {Value: &openapi3.Schema{
				Type:        &openapi3.Types{openapi3.TypeString},
				Description: "Present only when no shell exists yet, saying so and how one is created. Absent otherwise.",
			}},
		},
		Required: []string{"content", "from_offset", "next_offset", "exists"},
	}}
}

func (h *tools) GetToolsForToolsByName(_ context.Context, name string) ([]taskengine.Tool, error) {
	specs := shellToolSpecs()
	all := make([]taskengine.Tool, 0, len(specs))
	for _, spec := range specs {
		all = append(all, spec.tool())
	}
	if name == ToolsProviderName || name == "" {
		return all, nil
	}
	for _, t := range all {
		if t.Function.Name == name {
			return []taskengine.Tool{t}, nil
		}
	}
	return nil, fmt.Errorf("shell_session: unknown tool %q", name)
}

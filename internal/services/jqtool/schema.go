package jqtool

// Tool schema. Kept terse: the deadline, caps, containment boundary and
// exactly-one-source rule all have errors that state them precisely when they
// bite, so they aren't spelled out here. The description does state the
// things no error would ever teach: that the tool never writes, that it isn't
// goja_eval, that its input is one file or one string, and that YAML is
// accepted — each a case where a model would otherwise succeed at the wrong
// thing rather than get a correcting error.

import (
	"context"
	"fmt"

	"github.com/contenox/beam/internal/kernel/taskengine"
)

const queryDescription = "Run a jq program over ONE JSON or YAML document and get the values it emits. " +
	"Point it at a workspace file with `path`, or pass the document itself as a string in `input` — exactly one of the two. " +
	"Use it instead of reading a large config file: `.tasks[]|select(.handler==\"tools\")|.id` costs a few tokens where the file costs thousands. " +
	"It NEVER writes: a filter like `.a = 1` returns a modified COPY as its result and the file on disk is untouched. " +
	"It is not goja_eval — reach for jq for declarative shape-work over one document (select, project, map, group_by, keys, length), and for goja_eval when you need imperative logic or to call another tool. " +
	"The input is one file or one string, not a pipe: never paste large tool output into `input`."

func queryTool() taskengine.Tool {
	return taskengine.Tool{
		Type: "function",
		Function: taskengine.FunctionTool{
			Name:        ToolQuery,
			Description: queryDescription,
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"filter": map[string]any{
						"type":        "string",
						"description": "The jq program, e.g. \".tasks[] | select(.handler==\\\"tools\\\") | .id\". Use \".\" for the whole document, \"keys\" for an object's fields, \"length\" to count.",
					},
					"path": map[string]any{
						"type":        "string",
						"description": "Document to query, relative to the workspace root. Mutually exclusive with input.",
					},
					"input": map[string]any{
						"type":        "string",
						"description": "The document itself, as a JSON or YAML string. Mutually exclusive with path. For small documents you already have — not for large pasted output.",
					},
					"format": map[string]any{
						"type":        "string",
						"enum":        []string{FormatJSON, FormatYAML},
						"description": "Parser to use. Default: from the file extension, then from the content.",
					},
					"max": map[string]any{
						"type":        "integer",
						"description": fmt.Sprintf("Maximum values to return (default %d, ceiling %d). The result says when it was capped.", defaultMaxResults, maxResultsCeiling),
					},
					"deadline_ms": map[string]any{
						"type":        "integer",
						"description": fmt.Sprintf("Time budget in milliseconds (default %d, ceiling %d).", DefaultDeadline.Milliseconds(), MaxDeadline.Milliseconds()),
					},
				},
				"required": []string{"filter"},
			},
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

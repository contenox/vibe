package jqtool

// ---------------------------------------------------------------------------
// Tool schema
//
// Terse by default, for the reason localtools/fs_schema.go documents at length:
// a description is paid on EVERY turn, while everything a long description
// pre-teaches is re-taught by the error message at the moment it fires, with the
// concrete value filled in. The deadline, the caps, the containment boundary and
// the exactly-one-source rule all have errors that state them precisely when
// they bite, so none of them are spelled out here.
//
// Four things ARE in the description anyway, because no error would ever teach
// them — they are the cases where a model does the WRONG thing successfully, or
// never calls the tool at all:
//
//   - IT NEVER WRITES. A model that believes `.version = "2"` edits the file
//     will call this tool and then report the file as changed. Nothing fails, so
//     nothing can teach it.
//   - IT IS NOT goja_eval. Both take a program and return a value, and a model
//     with both available needs one line to pick. Choosing wrongly costs a turn
//     and never errors.
//   - ITS INPUT IS ONE FILE OR ONE STRING. Without this stated, the failure mode
//     is a model pasting a megabyte of previous tool output into `input` —
//     spending exactly the tokens the tool exists to save. The cap does fire,
//     but only after the tokens are spent.
//   - YAML IS ACCEPTED. A model that assumes "jq means JSON" reads the YAML file
//     into context instead of querying it, and nothing about that is an error.
// ---------------------------------------------------------------------------

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

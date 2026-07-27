package gojatool

import (
	"context"
	"fmt"

	"github.com/contenox/beam/internal/kernel/taskengine"
	"github.com/getkin/kin-openapi/openapi3"
)

// ---------------------------------------------------------------------------
// Tool schemas
//
// Terse by default, for the reason localtools/fs_schema.go documents at length:
// a description is paid on EVERY turn, while everything a long description
// pre-teaches is re-taught by the error message at the moment it fires, with the
// concrete value filled in. The deadline, the output cap, the call-depth cap and
// the JSON-only rule all have errors that state them precisely when they bite,
// so none of them are spelled out here.
//
// Three things are in the schema anyway, because no error would ever teach them:
//
//   - THE COMPLETION VALUE. A program whose last statement is not an expression
//     returns null, successfully. Nothing fails, so nothing can teach it.
//   - host.tool AND ITS ADDRESS FORM. A model that does not know the bridge
//     exists never calls it, and never sees an error about it.
//   - WHAT host.tool HANDS BACK. A tool that answers in prose comes back as
//     {text: "…"}, not as a bare string, because prose written for a reader is
//     not a contract for a program (hostresult.go). This one IS taught by an
//     error when it bites — but it bites on the FIRST call of every script that
//     reads a file, and one clause here is cheaper than that turn.
//   - THE ABSENCE OF AMBIENT I/O. "js" makes models reach for fetch and require;
//     saying plainly that there is no network, no filesystem and no module
//     system converts a wasted turn into a correct first call.
// ---------------------------------------------------------------------------

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

// GetSchemasForSupportedTools returns no OpenAPI documents: goja is a local
// toolset with hand-written function schemas, exactly like local_fs, gointel and
// shell_session. The model-facing contract is GetToolsForToolsByName.
func (t *Toolset) GetSchemasForSupportedTools(context.Context) (map[string]*openapi3.T, error) {
	return map[string]*openapi3.T{}, nil
}

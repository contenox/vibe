package searchtool

import (
	"context"
	"fmt"

	"github.com/contenox/contenox/internal/kernel/taskengine"
)

// Terse by default: a description is paid on every turn, and most of what a
// long one pre-teaches is re-taught by the error or note when it matters.
// Four things are stated anyway since nothing later re-teaches them: what it
// searches (any language, not just Go), that it is not gointel, that it is
// not a live read, and that a missing index is an instruction, not a fault.

const searchToolDoc = "Semantic search over this workspace's INDEXED CONTENT — prose, markdown, configuration and source in ANY language — " +
	"for questions like \"where is retry backoff explained\" or \"what configures the embedding model\". " +
	"Returns ranked hits, each a file:line-range citation plus the matching text, so an answer can be attributed and the range re-read in full. " +
	"NOT the Go-structure tool: exact questions about Go (what type is this, who calls this, where is it declared) belong to the go_* tools, which answer from a type checker; this one answers by meaning and can be approximately right. " +
	"NOT a live filesystem read: hits come from the index `contenox index` built, so a file changed since is returned FLAGGED STALE and a file never indexed is simply absent — re-read a cited range before relying on it. " +
	"A workspace with no index is NOT an error: the result says so and names the command (`contenox index`) for the human to run."

func (h *tools) GetToolsForToolsByName(_ context.Context, name string) ([]taskengine.Tool, error) {
	allTools := []taskengine.Tool{
		{
			Type: "function",
			Function: taskengine.FunctionTool{
				Name:        ToolSearch,
				Description: searchToolDoc,
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"question": map[string]any{
							"type":        "string",
							"description": "Natural-language question about the workspace. Phrase it as the thing you want to find, not as keywords — the ranking is semantic.",
						},
						"top_k": map[string]any{
							"type":        "integer",
							"description": fmt.Sprintf("Maximum citations to return (default %d, ceiling %d). The result is also capped at ~%d tokens overall and says how many hits it withheld.", topKDefault, topKMax, resultTokenBudget),
						},
					},
					"required": []string{"question"},
				},
			},
		},
	}

	if name == ToolsProviderName || name == "" {
		return allTools, nil
	}
	for _, t := range allTools {
		if t.Function.Name == name {
			return []taskengine.Tool{t}, nil
		}
	}
	return nil, fmt.Errorf("searchtool: unknown tool %q", echoName(name))
}

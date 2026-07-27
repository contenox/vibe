package searchtool

import (
	"context"
	"fmt"

	"github.com/contenox/beam/internal/kernel/taskengine"
)

// ---------------------------------------------------------------------------
// Tool schema
//
// Terse by default, for the reason localtools/fs_schema.go and gointel/schema.go
// both document: a description is paid on EVERY turn, while most of what a long
// description pre-teaches is re-taught by the error or the note at the moment it
// matters, with the concrete value filled in.
//
// Four things are stated anyway, because none of them can be re-taught by
// something that fires later:
//
//   - WHAT IT SEARCHES (indexed content in any language, not just Go), because a
//     model that thinks this is a Go tool never asks it about a markdown doc;
//   - THAT IT IS NOT gointel, because two workspace-reading toolsets with
//     overlapping names is exactly how a model picks the wrong one;
//   - THAT IT IS NOT A LIVE READ, because a stale or absent answer read as a
//     live one is a silent wrong answer, not an error; and
//   - THAT A MISSING INDEX IS AN INSTRUCTION, because a model taught to treat it
//     as a broken tool will stop calling the tool for the whole session instead
//     of telling the human the one command that fixes it.
// ---------------------------------------------------------------------------

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

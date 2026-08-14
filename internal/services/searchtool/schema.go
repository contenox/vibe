package searchtool

import (
	"context"
	"fmt"

	"github.com/contenox/contenox/internal/kernel/taskengine"
	"github.com/getkin/kin-openapi/openapi3"
)

// Terse by default: a description is paid on every turn, and most of what a
// long one pre-teaches is re-taught by the error or note when it matters.
// Five things are stated anyway since nothing later re-teaches them: what it
// searches (any language, not just Go), that ranking is keyword AND meaning so
// an exact identifier is a good query, that it answers from text rather than a
// type checker, that it is not a live read, and that a missing index is an
// instruction, not a fault.
//
// The argument set is declared once (searchProperties) and rendered twice:
// into the model-facing descriptor and into the published OpenAPI contract, so
// the two cannot drift.

const searchToolDoc = "Hybrid search over this workspace's INDEXED CONTENT — prose, markdown, configuration and source in ANY language. " +
	"Ranks by KEYWORD and by MEANING together, so both an exact identifier (`ResolveHITLApprovalWithinBound`, `CONTENOX_ACP_CHAIN_PATH`, an error string) and a plain question (\"where is retry backoff explained\") are good queries; " +
	"an exact symbol is found by the keyword half even when nothing was written about it in words. " +
	"Returns ranked hits, each a file:line-range citation plus the matching text, so an answer can be attributed and the range re-read in full. " +
	"NOT a parser: it answers from indexed TEXT, never from a type checker, so it can be APPROXIMATELY RIGHT \u2014 a hit is a location to verify, not a proof. " +
	"NOT a live filesystem read: hits come from the index `contenox index` built, so a file changed since is returned FLAGGED STALE and a file never indexed is simply absent — re-read a cited range before relying on it. " +
	"A workspace with no index is NOT an error: the result says so and names the command (`contenox index`) for the human to run."

// searchProperty is one workspace_search argument: name, JSON Schema type, the
// description the model reads, and whether the tool refuses without it.
type searchProperty struct {
	name        string
	typ         string
	description string
	required    bool
}

// searchProperties is the single source of truth for workspace_search's
// arguments.
func searchProperties() []searchProperty {
	return []searchProperty{
		{
			name:        "question",
			typ:         "string",
			description: "What to find: a question (\"where is retry backoff explained\") or the exact identifier, config key or error string you are looking for. Ranking fuses keyword and meaning, so a rare symbol pasted verbatim is a strong query — include it as-is rather than describing it.",
			required:    true,
		},
		{
			name:        "top_k",
			typ:         "integer",
			description: fmt.Sprintf("Maximum citations to return (default %d, ceiling %d). The result is also capped at ~%d tokens overall and says how many hits it withheld.", topKDefault, topKMax, resultTokenBudget),
		},
	}
}

// searchRequired renders the table's required set, in table order.
func searchRequired() []string {
	var out []string
	for _, p := range searchProperties() {
		if p.required {
			out = append(out, p.name)
		}
	}
	return out
}

// searchToolParameters renders the table as the descriptor's JSON Schema —
// what actually reaches the provider.
func searchToolParameters() map[string]any {
	props := make(map[string]any, len(searchProperties()))
	for _, p := range searchProperties() {
		props[p.name] = map[string]any{"type": p.typ, "description": p.description}
	}
	return map[string]any{
		"type":       "object",
		"properties": props,
		"required":   searchRequired(),
	}
}

// searchRequestSchema renders the same table as the published OpenAPI request
// schema.
func searchRequestSchema() *openapi3.SchemaRef {
	props := make(map[string]*openapi3.SchemaRef, len(searchProperties()))
	for _, p := range searchProperties() {
		props[p.name] = &openapi3.SchemaRef{Value: &openapi3.Schema{
			Type:        &openapi3.Types{p.typ},
			Description: p.description,
		}}
	}
	return &openapi3.SchemaRef{Value: &openapi3.Schema{
		Type:       &openapi3.Types{openapi3.TypeObject},
		Properties: props,
		Required:   searchRequired(),
	}}
}

func (h *tools) GetToolsForToolsByName(_ context.Context, name string) ([]taskengine.Tool, error) {
	allTools := []taskengine.Tool{
		{
			Type: "function",
			Function: taskengine.FunctionTool{
				Name:        ToolSearch,
				Description: searchToolDoc,
				Parameters:  searchToolParameters(),
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

// GetSchemasForSupportedTools publishes the toolset's OpenAPI 3.1 contract:
// workspace_search's request and response. The request schema is rendered from
// the same property table the descriptor renders (searchProperties), so the
// declared contract and what the model receives cannot drift. The response is
// the Result payload — including the empty-hits-plus-note shape a workspace
// with no index answers with, which is a result and not an error.
func (h *tools) GetSchemasForSupportedTools(context.Context) (map[string]*openapi3.T, error) {
	schema := &openapi3.T{
		OpenAPI: "3.1.0",
		Info: &openapi3.Info{
			Title:       "Workspace Search Tools",
			Description: "Hybrid keyword-and-meaning search over the workspace's indexed content, in any language. The keyword half needs no embedding model, so a workspace with none still searches. Read-only, and not a live filesystem read: every hit comes from the snapshot `contenox index` built, a hit whose file changed since is returned flagged stale, and a workspace with no index answers with a note naming the command to run rather than an error.",
			Version:     "1.0.0",
		},
		Paths: openapi3.NewPaths(),
		Components: &openapi3.Components{
			Schemas: map[string]*openapi3.SchemaRef{
				"WorkspaceSearchRequest":  searchRequestSchema(),
				"WorkspaceSearchResponse": searchResponseSchema(),
			},
		},
	}
	return map[string]*openapi3.T{ToolsProviderName: schema}, nil
}

// searchHitSchema is one ranked citation (Hit).
func searchHitSchema() *openapi3.SchemaRef {
	return &openapi3.SchemaRef{Value: &openapi3.Schema{
		Type:        &openapi3.Types{openapi3.TypeObject},
		Description: "One ranked citation.",
		Properties: map[string]*openapi3.SchemaRef{
			"citation":   searchString("The \"path:startLine-endLine\" form — the part to copy into an answer."),
			"path":       searchString("Workspace-relative path of the file the chunk came from."),
			"start_line": searchInt("First line of the cited range, 1-based."),
			"end_line":   searchInt("Last line of the cited range, inclusive."),
			"score":      {Value: &openapi3.Schema{Type: &openapi3.Types{openapi3.TypeNumber}, Description: "The rank-fusion score, rounded to four decimal places — a sum over the keyword and meaning legs of 1/(60 + that leg's rank), not a similarity. Comparable within one result, not across questions."}},
			"stale":      {Value: &openapi3.Schema{Type: &openapi3.Types{openapi3.TypeBoolean}, Description: "True when the file changed after it was indexed, so this text may no longer be what is on disk. Absent means the file is unchanged since indexing."}},
			"text":       searchString("The matching chunk. Clipped to the per-hit cap when it is long, with a trailing count of the characters not shown."),
			"truncated":  {Value: &openapi3.Schema{Type: &openapi3.Types{openapi3.TypeBoolean}, Description: "True when this chunk was clipped. Absent means it is whole."}},
		},
		Required: []string{"citation", "path", "start_line", "end_line", "score", "text"},
	}}
}

// searchResponseSchema is the Result payload Exec returns as DataTypeJSON. A
// refused or failed call returns an error instead, so nothing here is a
// failure marker.
func searchResponseSchema() *openapi3.SchemaRef {
	return &openapi3.SchemaRef{Value: &openapi3.Schema{
		Type: &openapi3.Types{openapi3.TypeObject},
		Properties: map[string]*openapi3.SchemaRef{
			"question": searchString("The question that produced these citations, echoed back clamped, so a transcript stays attributable."),
			"hits":     {Value: &openapi3.Schema{Type: &openapi3.Types{openapi3.TypeArray}, Items: searchHitSchema(), Description: "The citations, best first. Empty when nothing matched and when the workspace has no index — note says which."}},
			"found":    searchInt("How many hits ranking produced."),
			"shown":    searchInt("How many of them fit the token budget and are in hits. Below found when the budget bit."),
			"stale":    searchInt("How many of the shown hits are flagged stale. Absent when none are."),
			"note":     searchString("What this answer covers and what it leaves out: that no index exists yet and the command to build one, that nothing matched and what the index does not cover, how many hits were withheld and how to narrow, that the question itself was clamped, and how many hits are stale. Absent when there is nothing to say."),
		},
		Required: []string{"question", "hits", "found", "shown"},
	}}
}

// searchString is the common described string property.
func searchString(description string) *openapi3.SchemaRef {
	return &openapi3.SchemaRef{Value: &openapi3.Schema{Type: &openapi3.Types{openapi3.TypeString}, Description: description}}
}

// searchInt is the common described integer property.
func searchInt(description string) *openapi3.SchemaRef {
	return &openapi3.SchemaRef{Value: &openapi3.Schema{Type: &openapi3.Types{openapi3.TypeInteger}, Description: description}}
}

// Package searchtool exposes the workspace semantic index to an agent as one
// local toolset, provider "workspace", one tool: workspace_search. It is
// deliberately thin — argument decoding, a token-budgeted payload, a schema
// — with all retrieval in workspaceindex. A workspace with no index is not
// a failure: the result's note tells the model to run `contenox index`.
package searchtool

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode"

	"github.com/contenox/beam/internal/services/workspaceindex"
)

// ToolsProviderName is the tools-provider key this package registers under.
const ToolsProviderName = "workspace"

// ToolSearch is the one tool. The name is the HITL policy key; renaming it is a
// policy change, not a refactor.
const ToolSearch = "workspace_search"

// toolNames is the declaration order used by Supports and the tool list.
var toolNames = []string{ToolSearch}

// Querier is the one method this toolset needs from the index.
// workspaceindex.Service satisfies it. Narrow on purpose: nothing here may
// build, re-index, or otherwise spend.
type Querier interface {
	Query(ctx context.Context, workspaceID string, question string, topK int) ([]workspaceindex.Hit, error)
}

const (
	// topKDefault / topKMax bound how many citations come back; small
	// because each hit carries its chunk text.
	topKDefault = 5
	topKMax     = 20

	// resultTokenBudget is the ceiling on returned text, in tokens: an
	// uncapped result is a permanent tax on every later turn.
	resultTokenBudget = 1200

	// runesPerToken converts that budget to runes, mirroring the
	// ollamatokenizer estimator workspaceindex chunks against.
	runesPerToken = 4

	// resultRuneBudget / hitRuneCap are the derived caps: the total across
	// all hits, and the most one hit may spend, so one long chunk cannot
	// crowd out the others.
	resultRuneBudget = resultTokenBudget * runesPerToken
	hitRuneCap       = 1200

	// maxQuestionRunes bounds the question sent to the embedding model,
	// which is model-written and paid per call.
	maxQuestionRunes = 1000

	// maxEchoRunes bounds how much of a model-supplied string any result or error quotes back.
	maxEchoRunes = 120
)

// Severity markers, localtools' fatal-vs-recoverable convention. Every
// failure here is recoverable by a corrected call; "no index" is not an
// error at all.
const (
	severityRecoverable = "(recoverable: adjust parameters and retry)"
	severityFatalToken  = "(fatal:"
)

// noIndexNote is what a workspace with no index answers with: a result, not
// an error, naming the fix.
const noIndexNote = "No index exists for this workspace yet, so nothing could be searched. " +
	"This is not a failure of the tool: ask the human to run `contenox index` in the workspace, " +
	"and use the Go tools or a file read in the meantime."

// Hit is one ranked citation returned to the model.
type Hit struct {
	// Citation is the "path:startLine-endLine" form, the part copied into an answer.
	Citation  string  `json:"citation"`
	Path      string  `json:"path"`
	StartLine int     `json:"start_line"`
	EndLine   int     `json:"end_line"`
	Score     float64 `json:"score"`
	// Stale means the file changed after indexing; still returned, never as if current.
	Stale bool   `json:"stale,omitempty"`
	Text  string `json:"text"`
	// Truncated marks a chunk clipped to the per-hit cap.
	Truncated bool `json:"truncated,omitempty"`
}

// Result is the tool's payload. Found vs Shown is the honesty pair: Found is
// what ranking produced, Shown is what fit in the token budget, and Note says
// so explicitly whenever they differ.
type Result struct {
	Question string `json:"question"`
	Hits     []Hit  `json:"hits"`
	Found    int    `json:"found"`
	Shown    int    `json:"shown"`
	Stale    int    `json:"stale,omitempty"`
	Note     string `json:"note,omitempty"`
}

// clampTopK bounds the requested result count rather than refusing an
// out-of-range one: a model asking for 10000 hits means "as many as you have".
func clampTopK(topK int) int {
	if topK <= 0 {
		return topKDefault
	}
	if topK > topKMax {
		return topKMax
	}
	return topK
}

// appendNote joins result notes with a single space, so a result that is both
// stale AND truncated says both things in one field.
func appendNote(existing, add string) string {
	if add == "" {
		return existing
	}
	if existing == "" {
		return add
	}
	return existing + " " + add
}

// echoArg renders a model-supplied argument for an error message: clamped, then
// Go-quoted so control characters, NULs and bidi overrides are escaped rather
// than embedded in the result.
func echoArg(s string) string {
	r := []rune(s)
	if len(r) > maxEchoRunes {
		return strconv.Quote(string(r[:maxEchoRunes])) + fmt.Sprintf("… (+%d more characters)", len(r)-maxEchoRunes)
	}
	return strconv.Quote(s)
}

// echoName renders a model-supplied IDENTIFIER (an argument NAME) unquoted, with
// non-printable runes replaced — the unknown-argument voice local_fs and gointel
// share reads as a bare comma-separated list.
func echoName(s string) string {
	var b strings.Builder
	for i, r := range []rune(s) {
		switch {
		case i >= maxEchoRunes:
			b.WriteString("…")
			return b.String()
		case unicode.IsPrint(r):
			b.WriteRune(r)
		default:
			b.WriteRune('?')
		}
	}
	return b.String()
}

// echoErr renders a wrapped lower-level error inside a teaching message, clamped
// for the same reason echoArg is: the wrapped text routinely embeds the very
// argument that failed.
func echoErr(err error) string {
	if err == nil {
		return ""
	}
	r := []rune(err.Error())
	if len(r) > maxEchoRunes {
		return string(r[:maxEchoRunes]) + "…"
	}
	return string(r)
}

// recoverablef builds a teaching error tagged recoverable-by-correction.
func recoverablef(format string, a ...any) error {
	return errors.New(fmt.Sprintf(format, a...) + " " + severityRecoverable)
}

// hasSeverityMarker reports whether s already carries a marker, so nothing is
// double-tagged and no fatal is downgraded to recoverable.
func hasSeverityMarker(s string) bool {
	return strings.Contains(s, severityRecoverable) || strings.Contains(s, severityFatalToken)
}

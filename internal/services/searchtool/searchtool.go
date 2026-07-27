// Package searchtool exposes the workspace semantic index to an AGENT: one
// local toolset, provider "workspace", one tool, workspace_search.
//
// It is the third surface on internal/services/workspaceindex, beside `contenox
// index` (which fills the index) and `contenox search` (the operator's read).
// This one is the model's read, and it is deliberately the thinnest of the
// three: the whole package is argument decoding, a token-budgeted result
// payload, and a schema. All retrieval lives in workspaceindex.
//
// Shape is gointel's, on purpose (internal/services/gointel/{tools.go,schema.go}):
// same ToolsRepo contract, same strict-NAMES/lenient-VALUES argument discipline,
// same localtools severity markers, same "terse schema, teaching errors" split.
// Two toolsets that read the workspace and cite locations should not have two
// different manners.
//
// The relationship to gointel is COMPLEMENTARY, and the tool description says so
// in the model's own context: gointel answers structural Go questions from a
// type checker with exact truth; this answers semantic questions over everything
// the index walked — markdown, configs, prose, any language — with ranked
// citations. Neither is a live filesystem read.
//
// Degradation is the blueprint's rule (docs/development/blueprints/workspace-index.md):
// a workspace with NO index is not a failure, it is a runnable instruction. The
// tool returns an ordinary result whose note says to run `contenox index`, so a
// model that asked a reasonable question is not taught that the tool is broken.
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

// ToolsProviderName is the tools-provider key this package registers under — the
// `tools` name a chain task, a runtime allowlist, or a HITL policy rule refers
// to. The seeded envelopes allow it whole: the toolset has exactly one operation
// and that operation is a read of files the agent may already read.
const ToolsProviderName = "workspace"

// ToolSearch is the one tool. The name is the HITL policy key; renaming it is a
// policy change, not a refactor.
const ToolSearch = "workspace_search"

// toolNames is the declaration order used by Supports and the tool list.
var toolNames = []string{ToolSearch}

// Querier is the ONE method this toolset needs from the index. workspaceindex's
// Service satisfies it, so production passes the real service unchanged — and
// every test in this package runs against a fake with no database, no embedding
// model and no network.
//
// Narrow on purpose: nothing here may build, re-index, or otherwise SPEND. A
// tool the model can call must not be able to start a thousand embed calls.
type Querier interface {
	Query(ctx context.Context, workspaceID string, question string, topK int) ([]workspaceindex.Hit, error)
}

const (
	// topKDefault / topKMax bound how many citations come back. The default is
	// small because each hit carries its chunk text: five well-ranked passages
	// is what a model reads, and twenty is already past the point where the
	// token budget below starts dropping them anyway.
	topKDefault = 5
	topKMax     = 20

	// resultTokenBudget is the ceiling on the TEXT this tool returns in one
	// call, in tokens. The tool answers on every turn it is called and its
	// payload is paid for on every subsequent turn of the same conversation, so
	// an uncapped result is a permanent tax charged by a model-chosen argument.
	resultTokenBudget = 1200

	// runesPerToken converts that budget to runes. Four is the estimator's own
	// working assumption (internal/models/ollamatokenizer's estimator, which
	// workspaceindex chunks against), reused here rather than taking a
	// tokenizer dependency for a cap that only has to be approximately right.
	runesPerToken = 4

	// resultRuneBudget / hitRuneCap are the derived caps: the total across all
	// hits, and the most any single hit may spend of it. The per-hit cap exists
	// so one long chunk cannot consume the whole budget and hide the four other
	// citations behind it.
	resultRuneBudget = resultTokenBudget * runesPerToken
	hitRuneCap       = 1200

	// maxQuestionRunes bounds the question actually sent to the embedding model.
	// The argument is model-written, and an embedding call is paid per call
	// against a hosted provider; a question longer than this is not a question.
	maxQuestionRunes = 1000

	// maxEchoRunes bounds how much of a model-supplied string any RESULT or
	// ERROR quotes back — same reason and same number as gointel's: an echoed
	// argument is an output channel whose length the model controls.
	maxEchoRunes = 120
)

// Severity markers, localtools' fatal-vs-recoverable convention
// (internal/services/localtools/hardening.go). Every failure this toolset can
// produce is recoverable by a corrected call: there is no environment state a
// caller could not fix by asking differently, because "no index" is not an
// error here at all.
const (
	severityRecoverable = "(recoverable: adjust parameters and retry)"
	severityFatalToken  = "(fatal:"
)

// noIndexNote is what a workspace with no index answers with. It is a RESULT,
// not an error, and it names the command that fixes it — the blueprint's
// "retrieval is optional; its absence degrades, never fails" rule, rendered for
// a reader that cannot run the command itself but can tell the human to.
const noIndexNote = "No index exists for this workspace yet, so nothing could be searched. " +
	"This is not a failure of the tool: ask the human to run `contenox index` in the workspace, " +
	"and use the Go tools or a file read in the meantime."

// Hit is one ranked citation returned to the model.
type Hit struct {
	// Citation is the "path:startLine-endLine" form, first because it is the
	// part that gets copied into an answer.
	Citation  string  `json:"citation"`
	Path      string  `json:"path"`
	StartLine int     `json:"start_line"`
	EndLine   int     `json:"end_line"`
	Score     float64 `json:"score"`
	// Stale means the file changed after it was indexed. The hit is still
	// returned — the text may well still be the right passage — but never as if
	// it were current.
	Stale bool   `json:"stale,omitempty"`
	Text  string `json:"text"`
	// Truncated marks a chunk clipped to the per-hit cap, so a model never
	// treats a cut passage as a complete one.
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

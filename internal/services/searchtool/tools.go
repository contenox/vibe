package searchtool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/contenox/beam/internal/kernel/taskengine"
	"github.com/contenox/beam/internal/services/workspaceindex"
	"github.com/getkin/kin-openapi/openapi3"
)

// tools implements taskengine.ToolsRepo over a Querier. Dispatch follows
// gointel's, which follows localtools.LocalFSTools.execDispatch: accept args
// from the chain input map or from the declarative ToolsCall.Args, reject
// unknown argument NAMES, then hand off to a typed handler.
type tools struct {
	q           Querier
	workspaceID string
}

// NewTools returns the workspace-search ToolsRepo.
//
// It takes the narrow Querier rather than *workspaceindex.Service so this
// package never depends on a store, an embedding model, or a database — and so
// every test here runs offline. Register it in the engine's local tools map
// under ToolsProviderName the way gointel and local_fs are registered, so it is
// HITL-wrapped like every other toolset.
//
// workspaceID is fixed at construction because it is a property of the PROCESS's
// workspace, not of a call: letting the model name the workspace would make one
// project's index readable from another's session, which is the one containment
// property this feature has.
func NewTools(q Querier, workspaceID string) taskengine.ToolsRepo {
	return &tools{q: q, workspaceID: workspaceID}
}

func (h *tools) Exec(ctx context.Context, _ time.Time, input any, _ bool, call *taskengine.ToolsCall) (any, taskengine.DataType, error) {
	if call == nil {
		return nil, taskengine.DataTypeAny, errors.New("searchtool: tools required")
	}
	args, err := callArgs(input, call)
	if err != nil {
		return nil, taskengine.DataTypeAny, err
	}
	toolName := call.ToolName
	if toolName == "" {
		toolName = call.Name
	}

	switch toolName {
	case ToolSearch:
		if err := rejectUnknownArgs(ToolSearch, args, "question", "top_k"); err != nil {
			return nil, taskengine.DataTypeAny, err
		}
		topK, _ := argInt(args, "top_k")
		res, err := h.search(ctx, argString(args, "question"), topK)
		if err != nil {
			return nil, taskengine.DataTypeAny, err
		}
		return res, taskengine.DataTypeJSON, nil

	default:
		return nil, taskengine.DataTypeAny, fmt.Errorf("searchtool: unknown tool %q; this toolset provides %s %s",
			echoName(toolName), strings.Join(toolNames, ", "), severityRecoverable)
	}
}

func (h *tools) Supports(context.Context) ([]string, error) {
	return append([]string{ToolsProviderName}, toolNames...), nil
}

// GetSchemasForSupportedTools returns no OpenAPI documents: this is a local
// toolset with a hand-written function schema, exactly like local_fs, gointel
// and shell_session. The model-facing contract is GetToolsForToolsByName.
func (h *tools) GetSchemasForSupportedTools(context.Context) (map[string]*openapi3.T, error) {
	return map[string]*openapi3.T{}, nil
}

// search runs the one query and renders the result payload.
//
// Three things happen here and nowhere else, and each is a rule from the
// blueprint rather than a preference:
//
//   - ErrNoIndex becomes a RESULT with a runnable instruction. A workspace
//     nobody has indexed is the normal state of a fresh install, not a fault.
//   - Every hit carries its file:line-range, so the model can hand a human a
//     location it can verify, or re-read the range with a file tool.
//   - The payload is capped in tokens and says what it withheld. A silent cut
//     would let a model conclude "there is nothing else" from a budget decision.
func (h *tools) search(ctx context.Context, question string, topK int) (*Result, error) {
	question = strings.TrimSpace(question)
	if question == "" {
		return nil, recoverablef("%s: question is required — pass a natural-language question about the workspace, e.g. {\"question\": \"where is retry backoff configured\"}", ToolSearch)
	}
	var note string
	if r := []rune(question); len(r) > maxQuestionRunes {
		note = appendNote(note, fmt.Sprintf("The question was truncated to %d characters before searching; ask something shorter and more specific.", maxQuestionRunes))
		question = string(r[:maxQuestionRunes])
	}

	hits, err := h.q.Query(ctx, h.workspaceID, question, clampTopK(topK))
	if err != nil {
		if errors.Is(err, workspaceindex.ErrNoIndex) {
			return &Result{Question: echoQuestion(question), Hits: []Hit{}, Note: appendNote(note, noIndexNote)}, nil
		}
		if errors.Is(err, workspaceindex.ErrEmptyQuestion) {
			return nil, recoverablef("%s: question %s has no searchable content", ToolSearch, echoArg(question))
		}
		// Everything else is a real failure of the index (a dimension mismatch
		// after the embedding model changed, a store error). It is still
		// recoverable in the marker's sense — the caller can ask differently or
		// tell the human what to run — and the underlying text already names
		// the fix where one exists.
		if hasSeverityMarker(err.Error()) {
			return nil, fmt.Errorf("%s: %w", ToolSearch, err)
		}
		return nil, fmt.Errorf("%s: %s %s", ToolSearch, echoErr(err), severityRecoverable)
	}

	res := renderHits(question, hits)
	res.Note = appendNote(note, res.Note)
	return res, nil
}

// renderHits turns ranked index hits into the capped payload. Split out from
// search so the budget, the truncation marker and the staleness accounting are
// testable without a Querier at all.
func renderHits(question string, hits []workspaceindex.Hit) *Result {
	res := &Result{
		Question: echoQuestion(question),
		Hits:     make([]Hit, 0, len(hits)),
		Found:    len(hits),
	}
	if len(hits) == 0 {
		res.Note = "No indexed chunk matched. The index covers only what `contenox index` walked — gitignored paths, binaries and oversized files are excluded — and it is a snapshot, not a live read: content added since the last index is not here."
		return res
	}

	budget := resultRuneBudget
	stale := 0
	for _, hit := range hits {
		text, truncated, spent := clipText(hit.Text, budget)
		if text == "" {
			// The budget is gone; every remaining hit is withheld, and the note
			// below says exactly how many and why.
			break
		}
		budget -= spent
		if hit.Stale {
			stale++
		}
		res.Hits = append(res.Hits, Hit{
			Citation:  fmt.Sprintf("%s:%d-%d", hit.Path, hit.StartLine, hit.EndLine),
			Path:      hit.Path,
			StartLine: hit.StartLine,
			EndLine:   hit.EndLine,
			Score:     math.Round(hit.Score*1e4) / 1e4,
			Stale:     hit.Stale,
			Text:      text,
			Truncated: truncated,
		})
	}
	res.Shown = len(res.Hits)
	res.Stale = stale

	if withheld := res.Found - res.Shown; withheld > 0 {
		res.Note = appendNote(res.Note, fmt.Sprintf(
			"TRUNCATED: %d of %d hit(s) withheld — the ~%d-token result budget was reached. Ask a narrower question, or lower top_k and read the cited ranges directly.",
			withheld, res.Found, resultTokenBudget))
	}
	if stale > 0 {
		res.Note = appendNote(res.Note, fmt.Sprintf(
			"%d hit(s) are marked stale: the file changed after it was indexed, so the text shown may no longer be what is on disk. Re-read the cited range before relying on it, or ask the human to run `contenox index`.",
			stale))
	}
	return res
}

// clipText fits one chunk into what is left of the budget. It returns the text
// to show, whether it was cut, and how much of the budget it spent. An empty
// return means "this hit does not fit at all" — the caller stops there rather
// than emitting a citation with no text under it.
//
// The floor matters: a hit clipped below minHitRunes teaches nothing but costs a
// citation line, so the remaining budget is spent on nobody.
func clipText(text string, budget int) (string, bool, int) {
	const minHitRunes = 120
	if budget < minHitRunes {
		return "", false, 0
	}
	room := hitRuneCap
	if budget < room {
		room = budget
	}
	r := []rune(text)
	if len(r) <= room {
		return text, false, len(r)
	}
	return string(r[:room]) + fmt.Sprintf("\n… (+%d characters of this chunk not shown)", len(r)-room), true, room
}

// echoQuestion clamps the question echoed back in the result. The field is there
// so a model reading a transcript knows which question produced which citations;
// it is not there to carry a model-chosen payload back at full length.
func echoQuestion(q string) string {
	r := []rune(q)
	if len(r) > maxEchoRunes {
		return string(r[:maxEchoRunes]) + "…"
	}
	return q
}

// ---------------------------------------------------------------------------
// Argument decoding
//
// Verbatim gointel's (internal/services/gointel/tools.go), which is verbatim
// localtools/args.go's rule: argument NAMES are strict, argument VALUES are
// lenient. Small models routinely emit JSON scalars as strings ({"top_k": "3"}),
// and a strict type assertion silently drops the argument and answers a
// DIFFERENT question than the one asked.
// ---------------------------------------------------------------------------

// callArgs assembles the argument map from the chain input or, for declarative
// `tools` tasks that carry arguments on the call itself, from ToolsCall.Args.
func callArgs(input any, call *taskengine.ToolsCall) (map[string]any, error) {
	if m, ok := input.(map[string]any); ok && len(m) > 0 {
		return m, nil
	}
	if len(call.Args) > 0 {
		out := make(map[string]any, len(call.Args))
		for k, v := range call.Args {
			out[k] = v
		}
		return out, nil
	}
	if m, ok := input.(map[string]any); ok {
		return m, nil
	}
	return map[string]any{}, nil
}

func rejectUnknownArgs(toolName string, args map[string]any, allowed ...string) error {
	if len(args) == 0 {
		return nil
	}
	allowedSet := make(map[string]struct{}, len(allowed))
	for _, key := range allowed {
		allowedSet[key] = struct{}{}
	}
	var unknown []string
	for key := range args {
		if _, ok := allowedSet[key]; !ok {
			// The KEY is model-supplied too, so it is clamped like every other
			// echoed argument — an unknown-argument error must not be a channel
			// for a megabyte of model-chosen text.
			unknown = append(unknown, echoName(key))
		}
	}
	if len(unknown) == 0 {
		return nil
	}
	sort.Strings(unknown)
	sort.Strings(allowed)
	return fmt.Errorf("%s: unknown argument(s): %s (allowed: %s) %s",
		toolName, strings.Join(unknown, ", "), strings.Join(allowed, ", "), severityRecoverable)
}

func argString(args map[string]any, key string) string {
	x, ok := args[key]
	if !ok || x == nil {
		return ""
	}
	switch v := x.(type) {
	case string:
		return strings.TrimSpace(v)
	case json.Number:
		return v.String()
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64)
	case int:
		return strconv.Itoa(v)
	case int64:
		return strconv.FormatInt(v, 10)
	case bool:
		return strconv.FormatBool(v)
	}
	return ""
}

// intFromFloat converts a JSON number to an int WITHOUT the undefined behaviour
// Go's float→int conversion has outside the integer range. A model that emits
// 1e30 or NaN for top_k is not a hypothetical — it is one bad completion — and
// int(1e30) is unspecified, so the clamp happens here rather than downstream.
// Out-of-range saturates; NaN reads as "no value", which takes the default.
// clampTopK clamps again to this toolset's own ceiling.
func intFromFloat(f float64) (int, bool) {
	switch {
	case f != f: // NaN
		return 0, false
	case f >= float64(maxInt):
		return maxInt, true
	case f <= float64(minInt):
		return minInt, true
	}
	return int(f), true
}

const (
	maxInt = int(^uint(0) >> 1)
	minInt = -maxInt - 1
)

func argInt(args map[string]any, key string) (int, bool) {
	x, ok := args[key]
	if !ok || x == nil {
		return 0, false
	}
	switch v := x.(type) {
	case float64:
		return intFromFloat(v)
	case int:
		return v, true
	case int64:
		return int(v), true
	case json.Number:
		if f, err := v.Float64(); err == nil {
			return intFromFloat(f)
		}
	case string:
		s := strings.TrimSpace(v)
		if s == "" {
			return 0, false
		}
		if n, err := strconv.Atoi(s); err == nil {
			return n, true
		}
	}
	return 0, false
}

var _ taskengine.ToolsRepo = (*tools)(nil)

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

	"github.com/contenox/contenox/internal/kernel/taskengine"
	"github.com/contenox/contenox/internal/services/workspaceindex"
)

// tools implements taskengine.ToolsRepo over a Querier: args come from the
// chain input map or ToolsCall.Args, unknown argument names are rejected,
// then dispatch hands off to a typed handler.
type tools struct {
	q           Querier
	workspaceID string
}

// NewTools returns the workspace-search ToolsRepo. It takes the narrow
// Querier rather than *workspaceindex.Service so this package depends on no
// store, embedding model, or database. workspaceID is fixed at
// construction — a model naming its own workspace would make one project's
// index readable from another's session.
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

// search runs the one query and renders the result payload. ErrNoIndex
// becomes a result with a runnable instruction, not a fault; every hit
// carries its file:line-range; and the payload is capped in tokens and
// says what it withheld.
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
		// ErrIndexEmpty wraps ErrNoIndex, so it is matched first or it would
		// answer with the wrong one of the two notes.
		if errors.Is(err, workspaceindex.ErrIndexEmpty) {
			return &Result{Question: echoQuestion(question), Hits: []Hit{}, Note: appendNote(note, emptyIndexNote)}, nil
		}
		if errors.Is(err, workspaceindex.ErrNoIndex) {
			return &Result{Question: echoQuestion(question), Hits: []Hit{}, Note: appendNote(note, noIndexNote)}, nil
		}
		if errors.Is(err, workspaceindex.ErrEmptyQuestion) {
			return nil, recoverablef("%s: question %s has no searchable content", ToolSearch, echoArg(question))
		}
		// Everything else is a real index failure, still recoverable in the
		// marker's sense; the underlying text already names the fix where one exists.
		if hasSeverityMarker(err.Error()) {
			return nil, fmt.Errorf("%s: %w", ToolSearch, err)
		}
		return nil, fmt.Errorf("%s: %s %s", ToolSearch, echoErr(err), severityRecoverable)
	}

	res := renderHits(question, hits)
	res.Note = appendNote(note, res.Note)
	return res, nil
}

// renderHits turns ranked index hits into the capped payload, split out
// from search so the budget/truncation/staleness logic is testable without a Querier.
func renderHits(question string, hits []workspaceindex.Hit) *Result {
	res := &Result{
		Question: echoQuestion(question),
		Hits:     make([]Hit, 0, len(hits)),
		Found:    len(hits),
	}
	if len(hits) == 0 {
		res.Note = "No indexed chunk matched. Retry with the exact identifier, path fragment or error string if you have one — the keyword half of the ranking matches those literally. " +
			"The index covers only what `contenox index` walked — gitignored paths, binaries and oversized files are excluded — and it is a snapshot, not a live read: content added since the last index is not here."
		return res
	}

	budget := resultRuneBudget
	stale := 0
	for _, hit := range hits {
		text, truncated, spent := clipText(hit.Text, budget)
		if text == "" {
			// Budget is gone; remaining hits are withheld (see the note below).
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

// clipText fits one chunk into what is left of the budget, returning the
// text to show, whether it was cut, and how much budget it spent. An empty
// return means the hit does not fit at all. A hit clipped below
// minHitRunes teaches nothing but still costs a citation line, so it is
// dropped instead.
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

// --- Argument decoding: names are strict, values are lenient, since small
// models routinely emit JSON scalars as strings ({"top_k": "3"}). ---

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
			// The key is model-supplied too, so it is clamped like every echoed argument.
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

// intFromFloat converts a JSON number to an int without Go's undefined
// float->int behavior outside the integer range: out-of-range saturates,
// NaN reads as "no value" (the default). clampTopK clamps again to this
// toolset's own ceiling.
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

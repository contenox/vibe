package planservice

import (
	"encoding/json"
	"strings"

	"github.com/contenox/contenox/runtime/taskengine"
)

// formatTaskOutput collapses a task engine chain result into one string stored in
// planstore.PlanStep.ExecutionResult and surfaced as {{var:previous_output}} for the next step’s seed prompt.
//
// Contract (explicit — callers must not assume more):
//
//   - Purpose: a human-readable snapshot for the plan markdown sync and the next step’s prompt,
//     not a lossless transcript of the run.
//   - taskengine.ChatHistory: persisted text is chatHistoryForPlanStepPersistedResult (see there).
//   - string: returned as-is (no interpretation).
//   - map[string]any, default: JSON pretty-print for debugging/storage only.
//   - []any: strings joined with newlines; non-strings JSON-marshaled per element.
//
// Not guaranteed: tool result bodies, full multi-turn history, parity with files written to disk,
// or semantic “completion” of the step goal — only what this function returns.
func formatTaskOutput(out any) string {
	switch v := out.(type) {
	case string:
		return v

	case taskengine.ChatHistory:
		return chatHistoryForPlanStepPersistedResult(v)

	case map[string]any:
		b, _ := json.MarshalIndent(v, "", "  ")
		return string(b)

	case []any:
		var parts []string
		for _, item := range v {
			if s, ok := item.(string); ok {
				parts = append(parts, s)
			} else {
				b, _ := json.MarshalIndent(item, "", "  ")
				parts = append(parts, string(b))
			}
		}
		return strings.Join(parts, "\n")

	default:
		b, _ := json.MarshalIndent(v, "", "  ")
		return string(b)
	}
}

// chatHistoryForPlanStepPersistedResult defines the only rule used when the chain ends with
// ChatHistory: take the last assistant message with non-empty Content, scanning from the end.
// If none, use the last message’s Content (any role). If the model’s final turn is only "===STEP_DONE==="
// after writing files in earlier turns, that marker is what gets stored — not prior assistant text
// and not tool messages.
func chatHistoryForPlanStepPersistedResult(h taskengine.ChatHistory) string {
	if len(h.Messages) == 0 {
		return ""
	}
	for i := len(h.Messages) - 1; i >= 0; i-- {
		if h.Messages[i].Role == "assistant" && h.Messages[i].Content != "" {
			return h.Messages[i].Content
		}
	}
	return h.Messages[len(h.Messages)-1].Content
}

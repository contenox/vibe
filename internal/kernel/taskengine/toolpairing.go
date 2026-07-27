package taskengine

import (
	"time"

	"github.com/google/uuid"
)

// interruptedToolCallResult is the stub content recorded for a tool call that
// never received a result (interrupted execution, chain failure between the
// call and its execution, or a history routed out of a failure state).
const interruptedToolCallResult = "tool call was interrupted before a result was recorded"

// repairToolCallPairing enforces the tool-call protocol invariant on a
// transcript: every assistant tool call is answered by exactly one tool
// result, and every tool result answers a preceding assistant tool call.
// Providers with strict pairing (OpenAI Responses, Anthropic, Bedrock) reject
// conversations that violate this with a 400, so any history that leaves the
// engine — to a provider or to session persistence — must satisfy it.
//
// Repairs applied, in order:
//   - tool results whose call ID matches no assistant tool call are dropped
//   - duplicate tool results for the same call ID are dropped (first wins)
//   - assistant tool calls with no result get a stub error result inserted
//     directly after the contiguous tool-result block that follows them
func repairToolCallPairing(msgs []Message) []Message {
	callIDs := map[string]bool{}
	for _, m := range msgs {
		if m.Role != "assistant" {
			continue
		}
		for _, tc := range m.CallTools {
			if tc.ID != "" {
				callIDs[tc.ID] = true
			}
		}
	}

	answered := map[string]bool{}
	kept := make([]Message, 0, len(msgs))
	changed := false
	for _, m := range msgs {
		if m.Role == "tool" {
			if m.ToolCallID == "" || !callIDs[m.ToolCallID] || answered[m.ToolCallID] {
				changed = true
				continue
			}
			answered[m.ToolCallID] = true
		}
		kept = append(kept, m)
	}

	out := make([]Message, 0, len(kept))
	for i := 0; i < len(kept); i++ {
		m := kept[i]
		out = append(out, m)
		if m.Role != "assistant" || len(m.CallTools) == 0 {
			continue
		}
		for i+1 < len(kept) && kept[i+1].Role == "tool" {
			i++
			out = append(out, kept[i])
		}
		for _, tc := range m.CallTools {
			if tc.ID == "" || answered[tc.ID] {
				continue
			}
			answered[tc.ID] = true
			changed = true
			out = append(out, Message{
				ID:         uuid.NewString(),
				Role:       "tool",
				ToolCallID: tc.ID,
				Content:    toolErrorContent(interruptedToolCallResult),
				Timestamp:  time.Now().UTC(),
			})
		}
	}

	if !changed {
		return msgs
	}
	return out
}

// resumableToolCallBatch locates the OPEN tool-call batch at the tail of a
// transcript: the last assistant message carrying tool calls, tolerating a
// trailing block of tool results that answer SOME of them. It returns the
// assistant message's index and the set of call IDs those trailing results
// already answered, or (-1, nil) when the transcript does not end in a batch
// with at least one unanswered call.
//
// The tolerance is what makes a suspended run's checkpoint re-enterable: the
// interrupted batch persists as "assistant calls + partial results", and the
// resume path re-runs execute_tool_calls over exactly that shape, executing
// only the calls the trailing results have not answered. For an untouched
// transcript (last message IS the assistant call) it degenerates to the
// pre-S6 behavior: index len-1, nothing answered.
func resumableToolCallBatch(msgs []Message) (assistantIdx int, answered map[string]bool) {
	i := len(msgs) - 1
	answered = map[string]bool{}
	for i >= 0 && msgs[i].Role == "tool" {
		if msgs[i].ToolCallID != "" {
			answered[msgs[i].ToolCallID] = true
		}
		i--
	}
	if i < 0 || msgs[i].Role != "assistant" || len(msgs[i].CallTools) == 0 {
		return -1, nil
	}
	for _, tc := range msgs[i].CallTools {
		// An ID-less call can never be matched by a result, so it counts as
		// unanswered (mirroring the execution loop, which runs it).
		if tc.ID == "" || !answered[tc.ID] {
			return i, answered
		}
	}
	// Every call of the batch is already answered: nothing to execute.
	return -1, nil
}

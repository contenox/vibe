package taskengine

import (
	"time"

	"github.com/google/uuid"
)

const interruptedToolCallResult = "tool call was interrupted before a result was recorded"

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
		// An ID-less call can never be matched by a result, so it always counts as unanswered.
		if tc.ID == "" || !answered[tc.ID] {
			return i, answered
		}
	}
	return -1, nil
}

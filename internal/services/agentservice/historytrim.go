package agentservice

import "github.com/contenox/contenox/internal/kernel/taskengine"

func trimHistoryChunked(history []taskengine.Message, budget int) []taskengine.Message {
	if budget <= 0 || len(history) <= budget {
		return history
	}

	pinned := 0
	for pinned < len(history) && history[pinned].Role == "system" {
		pinned++
	}
	if pinned >= budget {
		// pinned alone fills the budget: fall back to the plain window so budget still wins
		return history[len(history)-budget:]
	}

	chunk := budget / 4
	if chunk < 1 {
		chunk = 1
	}
	target := budget - chunk
	if target < pinned+1 {
		target = pinned + 1
	}

	keepTail := target - pinned
	// shrink until the tail doesn't open on an orphaned tool-result
	for keepTail > 1 && history[len(history)-keepTail].Role == "tool" {
		keepTail--
	}

	out := make([]taskengine.Message, 0, pinned+keepTail)
	out = append(out, history[:pinned]...)
	out = append(out, history[len(history)-keepTail:]...)
	return out
}

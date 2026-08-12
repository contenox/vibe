package agentservice

import "github.com/contenox/contenox/internal/kernel/taskengine"

// trimHistoryChunked enforces the HistoryTrim message budget, cutting to budget minus one chunk (25%) so the kept prefix stays byte-identical across turns until it must move; leading system messages are pinned and the kept tail never opens on an orphaned "tool" message.
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

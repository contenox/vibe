package agentservice

import "github.com/contenox/beam/internal/kernel/taskengine"

// trimHistoryChunked enforces the HistoryTrim message budget the cache-stable
// way (provider-kv-cache blueprint §4.1(3), E3).
//
// The old sliding window ("keep the last N") moved the first history message
// on every turn once the session exceeded N, which re-cold-started every
// provider prefix cache each turn — the dominant cost of an agent loop. This
// version keeps trimming an *event* instead of a drift:
//
//   - Within budget nothing changes, so the history prefix grows by appendage
//     only and stays byte-identical across turns.
//   - When the budget is exceeded, it trims below budget by one chunk (25% of
//     the budget), buying roughly budget/4 warm append-only turns per single
//     cold trim instead of a cold start on every turn.
//   - Leading system messages (the AGENTS.md project-context message) are
//     pinned: in contextasm.CacheClass terms they are the task-pinned tier
//     and chat turns are the volatile tier, so volatile messages are dropped
//     first (heritage rule). The sliding window used to silently discard the
//     AGENTS.md message forever — it is only injected while history is empty.
//   - The cut never leaves an orphaned tool-result at the head of the kept
//     tail: a "tool" message without its preceding assistant tool-call is a
//     protocol error on several providers.
//
// The budget stays an upper bound exactly as before: the returned history
// never exceeds budget messages.
func trimHistoryChunked(history []taskengine.Message, budget int) []taskengine.Message {
	if budget <= 0 || len(history) <= budget {
		return history
	}

	// Pinned prefix: leading system messages (project context/conventions).
	pinned := 0
	for pinned < len(history) && history[pinned].Role == "system" {
		pinned++
	}
	if pinned >= budget {
		// Degenerate: the pinned prefix alone fills the budget. Honor the
		// budget over pinning — fall back to the plain window so the newest
		// turns (including the conversation the user is having right now)
		// survive.
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
	// Never start the kept tail on a tool-result whose assistant tool-call
	// was dropped; shrink the tail until it opens on a safe role.
	for keepTail > 1 && history[len(history)-keepTail].Role == "tool" {
		keepTail--
	}

	out := make([]taskengine.Message, 0, pinned+keepTail)
	out = append(out, history[:pinned]...)
	out = append(out, history[len(history)-keepTail:]...)
	return out
}

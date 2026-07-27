package agentservice

import (
	"fmt"
	"testing"

	"github.com/contenox/beam/internal/kernel/taskengine"
)

func msg(role, content string) taskengine.Message {
	return taskengine.Message{ID: content, Role: role, Content: content}
}

func chatHistory(n int) []taskengine.Message {
	out := make([]taskengine.Message, 0, n)
	for i := 0; i < n; i++ {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		out = append(out, msg(role, fmt.Sprintf("m%d", i)))
	}
	return out
}

func TestUnit_TrimHistoryChunked_NoTrimWithinBudget(t *testing.T) {
	h := chatHistory(10)
	got := trimHistoryChunked(h, 10)
	if len(got) != 10 {
		t.Fatalf("within budget must be untouched, got %d messages", len(got))
	}
	got = trimHistoryChunked(h, 0)
	if len(got) != 10 {
		t.Fatalf("budget 0 disables trimming, got %d messages", len(got))
	}
}

func TestUnit_TrimHistoryChunked_BudgetHonoredAndChunked(t *testing.T) {
	h := chatHistory(30)
	got := trimHistoryChunked(h, 20)
	if len(got) > 20 {
		t.Fatalf("budget is an upper bound: got %d > 20", len(got))
	}
	// Chunked semantics: trims BELOW budget (budget - budget/4 = 15) so the
	// next ~5 turns append without moving the prefix boundary.
	if len(got) != 15 {
		t.Fatalf("expected trim to 15 (budget 20 minus 25%% chunk), got %d", len(got))
	}
	// Newest messages survive.
	if got[len(got)-1].Content != "m29" {
		t.Fatalf("newest message must survive the trim, got %s", got[len(got)-1].Content)
	}
}

func TestUnit_TrimHistoryChunked_BoundaryStableAcrossTurns(t *testing.T) {
	h := chatHistory(21)
	trimmed := trimHistoryChunked(h, 20)
	first := trimmed[0].Content

	// Simulate the next few turns: append two messages per turn; until the
	// budget is exceeded again the earlier prefix must not move.
	for turn := 0; turn < 2; turn++ {
		trimmed = append(trimmed, msg("user", fmt.Sprintf("u%d", turn)), msg("assistant", fmt.Sprintf("a%d", turn)))
		next := trimHistoryChunked(trimmed, 20)
		if len(next) > 20 {
			t.Fatalf("budget exceeded after turn %d: %d", turn, len(next))
		}
		if next[0].Content != first {
			t.Fatalf("prefix boundary moved on an in-budget turn %d: %s -> %s (cold cache every turn is the bug this trim exists to fix)", turn, first, next[0].Content)
		}
		trimmed = next
	}
}

func TestUnit_TrimHistoryChunked_PinsLeadingSystemMessages(t *testing.T) {
	h := append([]taskengine.Message{msg("system", "agents-md")}, chatHistory(30)...)
	got := trimHistoryChunked(h, 20)
	if got[0].Role != "system" || got[0].Content != "agents-md" {
		t.Fatalf("leading system message (AGENTS.md project context) must be pinned through trims, got head %s/%s", got[0].Role, got[0].Content)
	}
	if len(got) > 20 {
		t.Fatalf("budget is an upper bound: got %d", len(got))
	}
}

func TestUnit_TrimHistoryChunked_NeverOpensTailOnToolResult(t *testing.T) {
	h := chatHistory(40)
	// Place a tool-result exactly where the naive cut would land so the guard
	// has to shrink the tail. With budget 20 the naive tail keeps 15 messages,
	// i.e. it would open on index len-15.
	h[len(h)-15] = msg("tool", "orphaned-tool-result")
	got := trimHistoryChunked(h, 20)
	if got[0].Role == "tool" {
		t.Fatalf("kept tail must not open on an orphaned tool result")
	}
	if len(got) > 20 {
		t.Fatalf("budget is an upper bound: got %d", len(got))
	}
}

func TestUnit_TrimHistoryChunked_PinnedPrefixFillingBudgetFallsBack(t *testing.T) {
	h := make([]taskengine.Message, 0, 12)
	for i := 0; i < 6; i++ {
		h = append(h, msg("system", fmt.Sprintf("s%d", i)))
	}
	h = append(h, chatHistory(6)...)
	got := trimHistoryChunked(h, 4)
	if len(got) != 4 {
		t.Fatalf("degenerate pinned>=budget case must honor the budget, got %d", len(got))
	}
	if got[len(got)-1].Content != "m5" {
		t.Fatalf("newest message must survive, got %s", got[len(got)-1].Content)
	}
}

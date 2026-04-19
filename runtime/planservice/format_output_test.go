package planservice

import (
	"testing"
	"time"

	"github.com/contenox/contenox/runtime/taskengine"
)

func TestChatHistoryForPlanStepPersistedResult_contract(t *testing.T) {
	t.Parallel()
	t.Run("empty_messages", func(t *testing.T) {
		if got := chatHistoryForPlanStepPersistedResult(taskengine.ChatHistory{}); got != "" {
			t.Fatalf("got %q want empty", got)
		}
	})
	t.Run("last_assistant_wins", func(t *testing.T) {
		h := taskengine.ChatHistory{
			Messages: []taskengine.Message{
				{Role: "user", Content: "go", Timestamp: time.Now().UTC()},
				{Role: "assistant", Content: "first draft", Timestamp: time.Now().UTC()},
				{Role: "assistant", Content: "===STEP_DONE===", Timestamp: time.Now().UTC()},
			},
		}
		if got := chatHistoryForPlanStepPersistedResult(h); got != "===STEP_DONE===" {
			t.Fatalf("got %q want final assistant only", got)
		}
	})
	t.Run("skips_empty_assistant_finds_earlier", func(t *testing.T) {
		h := taskengine.ChatHistory{
			Messages: []taskengine.Message{
				{Role: "assistant", Content: "substance", Timestamp: time.Now().UTC()},
				{Role: "assistant", Content: "", Timestamp: time.Now().UTC()},
			},
		}
		if got := chatHistoryForPlanStepPersistedResult(h); got != "substance" {
			t.Fatalf("got %q want earlier non-empty assistant", got)
		}
	})
	t.Run("no_assistant_non_empty_falls_back_to_last_message", func(t *testing.T) {
		h := taskengine.ChatHistory{
			Messages: []taskengine.Message{
				{Role: "user", Content: "only user", Timestamp: time.Now().UTC()},
			},
		}
		if got := chatHistoryForPlanStepPersistedResult(h); got != "only user" {
			t.Fatalf("got %q", got)
		}
	})
}

func TestFormatTaskOutput_stringPassthrough(t *testing.T) {
	t.Parallel()
	if got := formatTaskOutput("hello"); got != "hello" {
		t.Fatalf("got %q", got)
	}
}

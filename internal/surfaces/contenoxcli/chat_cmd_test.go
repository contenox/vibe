package contenoxcli

import (
	"strings"
	"testing"

	"github.com/contenox/contenox/internal/kernel/taskengine"
	"github.com/stretchr/testify/require"
)

// TestUnit_StreamOverlap asserts the reconciliation between what a live stream
// already put on screen and the buffered answer: the whole answer when the
// stream completed, the streamed prefix when it was cut mid-answer, and nothing
// when the deltas belonged to a different message.
func TestUnit_StreamOverlap(t *testing.T) {
	require.Equal(t, len("the answer"), streamOverlap("the answer", "the answer"))
	require.Equal(t, len("the answer"), streamOverlap("a preamble.the answer", "the answer"),
		"earlier assistant turns must not stop the tail from matching")
	require.Equal(t, len("the ans"), streamOverlap("a preamble.the ans", "the answer"),
		"a stream cut mid-answer overlaps only as far as it got")
	require.Equal(t, 0, streamOverlap("a preamble.", "the answer"))
	require.Equal(t, 0, streamOverlap("", "the answer"))
}

// TestUnit_PrintRemainingOutput asserts a streamed turn prints each byte of the
// answer exactly once, and that a non-streaming run is byte-identical to the
// old buffered path.
func TestUnit_PrintRemainingOutput(t *testing.T) {
	hist := taskengine.ChatHistory{Messages: []taskengine.Message{
		{Role: "user", Content: "hi"},
		{Role: "assistant", Content: "the answer"},
	}}

	t.Run("nothing streamed prints the whole answer", func(t *testing.T) {
		var out strings.Builder
		printRemainingOutput(&out, "", hist, taskengine.DataTypeChatHistory, false)
		require.Equal(t, "the answer\n", out.String())
	})

	t.Run("fully streamed prints only the terminating newline", func(t *testing.T) {
		var out strings.Builder
		printRemainingOutput(&out, "the answer", hist, taskengine.DataTypeChatHistory, false)
		require.Equal(t, "\n", out.String())
	})

	t.Run("partially streamed prints just the tail", func(t *testing.T) {
		var out strings.Builder
		printRemainingOutput(&out, "preamble.the ans", hist, taskengine.DataTypeChatHistory, false)
		require.Equal(t, "wer\n", out.String())
	})

	t.Run("unrelated deltas leave the answer to be printed in full", func(t *testing.T) {
		var out strings.Builder
		printRemainingOutput(&out, "reading main.go...", hist, taskengine.DataTypeChatHistory, false)
		require.Contains(t, out.String(), "the answer\n")
	})

	t.Run("a structured payload is never suppressed by narration", func(t *testing.T) {
		var out strings.Builder
		printRemainingOutput(&out, "narration", map[string]string{"k": "v"}, taskengine.DataTypeJSON, false)
		require.Contains(t, out.String(), `"k": "v"`)
	})
}

// TestUnit_AssistantStreamDisabled asserts the zero/disabled stream keeps the
// buffered path exactly as it was: no writes, no recorded output, and a Stop
// that is safe to call twice.
func TestUnit_AssistantStreamDisabled(t *testing.T) {
	s := startAssistantStream(t.Context(), nil, &strings.Builder{}, true)
	require.Empty(t, s.Written())
	s.Stop()
	s.Stop()
	require.Empty(t, s.Written())
}

package taskengine

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestUnit_EnsureFailureContext_EmptyHistoryReceivesErrorAsUserMessage(t *testing.T) {
	failure := errors.New("stream failed (provider=vertex-google, model=gemini-3.6-flash): 404")

	out, dataType := ensureFailureContext(ChatHistory{}, DataTypeChatHistory, "contenox_run", failure)
	require.Equal(t, DataTypeChatHistory, dataType)
	hist, ok := out.(ChatHistory)
	require.True(t, ok)
	require.Len(t, hist.Messages, 1)
	require.Equal(t, "user", hist.Messages[0].Role)
	require.Contains(t, hist.Messages[0].Content, failure.Error())
	require.Contains(t, hist.Messages[0].Content, "contenox_run")
	require.Zero(t, hist.InputTokens, "extended history must be recounted")
}

// An orphaned-tool-only tail counts as empty after pairing repair strips it.
func TestUnit_EnsureFailureContext_OrphanedToolTailReceivesErrorAsUserMessage(t *testing.T) {
	failure := errors.New("root cause")
	hist := ChatHistory{Messages: []Message{
		{Role: "tool", ToolCallID: "call-1", Content: "a large tool result whose assistant call was capped away"},
	}}

	out, _ := ensureFailureContext(hist, DataTypeChatHistory, "main", failure)
	got, ok := out.(ChatHistory)
	require.True(t, ok)
	last := got.Messages[len(got.Messages)-1]
	require.Equal(t, "user", last.Role)
	require.Contains(t, last.Content, failure.Error())
}

func TestUnit_EnsureFailureContext_ContentBearingHistoryUntouched(t *testing.T) {
	hist := ChatHistory{
		Messages:    []Message{{Role: "user", Content: "real context"}},
		InputTokens: 7,
	}

	out, dataType := ensureFailureContext(hist, DataTypeChatHistory, "main", errors.New("boom"))
	require.Equal(t, DataTypeChatHistory, dataType)
	got, ok := out.(ChatHistory)
	require.True(t, ok)
	require.Len(t, got.Messages, 1)
	require.Equal(t, "real context", got.Messages[0].Content)
	require.Equal(t, 7, got.InputTokens)
}

// An unanswered assistant tool call is reportable content (pairing repair
// stubs, not drops, its result).
func TestUnit_EnsureFailureContext_AssistantToolCallCountsAsContent(t *testing.T) {
	hist := ChatHistory{Messages: []Message{
		{Role: "assistant", CallTools: []ToolCall{{ID: "call-1"}}},
	}}

	out, _ := ensureFailureContext(hist, DataTypeChatHistory, "main", errors.New("boom"))
	got, ok := out.(ChatHistory)
	require.True(t, ok)
	require.Len(t, got.Messages, 1)
}

func TestUnit_EnsureFailureContext_StringInputs(t *testing.T) {
	failure := errors.New("boom")

	out, dataType := ensureFailureContext("  ", DataTypeString, "main", failure)
	require.Equal(t, DataTypeString, dataType)
	require.Contains(t, out.(string), failure.Error())

	out, _ = ensureFailureContext("real prompt", DataTypeString, "main", failure)
	require.Equal(t, "real prompt", out)
}

func TestUnit_EnsureFailureContext_NilFailureUntouched(t *testing.T) {
	out, dataType := ensureFailureContext(ChatHistory{}, DataTypeChatHistory, "main", nil)
	require.Equal(t, DataTypeChatHistory, dataType)
	hist, ok := out.(ChatHistory)
	require.True(t, ok)
	require.Empty(t, hist.Messages)
}

// Routed history sheds audio on a copy; the caller's original keeps it.
func TestUnit_EnsureFailureContext_AudioStrippedFromRoutedHistory(t *testing.T) {
	original := ChatHistory{Messages: []Message{
		{Role: "user", Content: "transcribe this recording", Audio: []AudioPart{{Data: []byte{1, 2, 3}, MimeType: "audio/wav"}}},
		{Role: "assistant", Content: "working on it"},
	}}

	out, dataType := ensureFailureContext(original, DataTypeChatHistory, "acp_chat",
		errors.New("no available model supports audio input"))
	require.Equal(t, DataTypeChatHistory, dataType)
	got, ok := out.(ChatHistory)
	require.True(t, ok)
	require.Len(t, got.Messages, 2, "content-bearing history still gets no notice")
	require.Equal(t, "transcribe this recording", got.Messages[0].Content)
	require.Empty(t, got.Messages[0].Audio,
		"the failure handler must not inherit the audio that may have caused the failure")
	require.NotEmpty(t, original.Messages[0].Audio,
		"the caller's history keeps its audio: the strip works on a copy")
}

// An audio-only history is effectively empty, so the original error still
// reaches the handler.
func TestUnit_EnsureFailureContext_AudioOnlyHistoryStillReportsTheError(t *testing.T) {
	failure := errors.New("no model matched the requirements: no available model supports audio input")
	hist := ChatHistory{Messages: []Message{
		{Role: "user", Audio: []AudioPart{{Data: []byte{1}, MimeType: "audio/wav"}}},
	}}

	out, dataType := ensureFailureContext(hist, DataTypeChatHistory, "acp_chat", failure)
	require.Equal(t, DataTypeChatHistory, dataType)
	got, ok := out.(ChatHistory)
	require.True(t, ok)
	last := got.Messages[len(got.Messages)-1]
	require.Equal(t, "user", last.Role)
	require.Contains(t, last.Content, failure.Error(), "the original error reaches the handler unmasked")
	require.Contains(t, last.Content, "acp_chat")
	for _, m := range got.Messages {
		require.Empty(t, m.Audio, "no rung of the ladder re-inherits the audio")
	}
}

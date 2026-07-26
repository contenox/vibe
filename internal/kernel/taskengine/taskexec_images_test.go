package taskengine

import (
	"testing"

	libmodelprovider "github.com/contenox/beam/internal/models/modelrepo"
	"github.com/stretchr/testify/require"
)

// TestUnit_ProviderMessagesFromEngine_ThreadsImages covers the engine->provider
// message conversion: image attachments must travel with their message, beside
// text and tool calls, and never leak into prelude messages.
func TestUnit_ProviderMessagesFromEngine_ThreadsImages(t *testing.T) {
	t.Parallel()

	prelude := []Message{
		{Role: "system", Content: "you are helpful"},
	}
	png := []byte{0x89, 0x50, 0x4e, 0x47}
	messages := []Message{
		{
			Role:    "user",
			Content: "what is in this screenshot?",
			Images: []ImagePart{
				{Data: png, MimeType: "image/png"},
			},
		},
		{
			Role: "assistant",
			CallTools: []ToolCall{
				{
					ID:           "call_1",
					Type:         "function",
					Function:     FunctionCall{Name: "read_file", Arguments: `{"path":"a.go"}`},
					ProviderMeta: map[string]string{"thought_signature": "sig"},
				},
			},
		},
		{Role: "tool", Content: "file contents", ToolCallID: "call_1"},
	}

	got := providerMessagesFromEngine(prelude, messages)
	require.Len(t, got, 4)

	require.Equal(t, "system", got[0].Role)
	require.Empty(t, got[0].Images)

	require.Equal(t, "user", got[1].Role)
	require.Equal(t, "what is in this screenshot?", got[1].Content)
	require.Len(t, got[1].Images, 1)
	require.Equal(t, png, got[1].Images[0].Data)
	require.Equal(t, "image/png", got[1].Images[0].MimeType)

	require.Equal(t, "assistant", got[2].Role)
	require.Len(t, got[2].ToolCalls, 1)
	require.Equal(t, "call_1", got[2].ToolCalls[0].ID)
	require.Equal(t, "read_file", got[2].ToolCalls[0].Function.Name)
	require.Equal(t, map[string]string{"thought_signature": "sig"}, got[2].ToolCalls[0].ProviderMeta)

	require.Equal(t, "tool", got[3].Role)
	require.Equal(t, "call_1", got[3].ToolCallID)

	require.True(t, libmodelprovider.MessagesHaveImages(got))
}

// TestUnit_ProviderMessagesFromEngine_TextOnlyStaysImageFree guards the common
// path: without attachments no message reports images, so resolution never
// demands a vision-capable model.
func TestUnit_ProviderMessagesFromEngine_TextOnlyStaysImageFree(t *testing.T) {
	t.Parallel()

	got := providerMessagesFromEngine(nil, []Message{
		{Role: "user", Content: "plain text"},
	})
	require.Len(t, got, 1)
	require.Nil(t, got[0].Images)
	require.False(t, libmodelprovider.MessagesHaveImages(got))
}

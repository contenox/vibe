package agentservice

import (
	"context"
	"testing"

	"github.com/contenox/beam/internal/kernel/taskengine"
	"github.com/stretchr/testify/require"
)

func TestUnit_BuildChatInput_UserMessageCarriesImages(t *testing.T) {
	img := taskengine.ImagePart{Data: []byte{0xFF, 0xD8, 0xFF}, MimeType: "image/jpeg"}

	input, dataType, err := (&agent{}).buildChatInput(context.Background(), PromptRequest{
		Input:  "what is in this picture?",
		Images: []taskengine.ImagePart{img},
	})
	require.NoError(t, err)
	require.Equal(t, taskengine.DataTypeChatHistory, dataType)

	history, ok := input.(taskengine.ChatHistory)
	require.True(t, ok)
	require.NotEmpty(t, history.Messages)

	// The attachment rides the user message of THIS turn — the shape every
	// provider codec consumes and llmresolver routes on (CanVision).
	userMsg := history.Messages[len(history.Messages)-1]
	require.Equal(t, "user", userMsg.Role)
	require.Equal(t, []taskengine.ImagePart{img}, userMsg.Images)
}

func TestUnit_BuildChatInput_ImageOnlyTurnIsValid(t *testing.T) {
	input, _, err := (&agent{}).buildChatInput(context.Background(), PromptRequest{
		Images: []taskengine.ImagePart{{Data: []byte{1}, MimeType: "image/png"}},
	})
	require.NoError(t, err)
	history := input.(taskengine.ChatHistory)
	userMsg := history.Messages[len(history.Messages)-1]
	require.Empty(t, userMsg.Content)
	require.Len(t, userMsg.Images, 1)
}

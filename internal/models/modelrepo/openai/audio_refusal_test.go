package openai

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/contenox/contenox/internal/models/modelrepo"
	"github.com/stretchr/testify/require"
)

// TestUnit_OpenAIChat_RefusesAudioBeforeWire checks an audio-bearing request is refused with the typed capability error before any bytes reach the backend.
func TestUnit_OpenAIChat_RefusesAudioBeforeWire(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("audio-bearing request must be refused before any HTTP call")
	}))
	defer srv.Close()

	client := newGPT5ChatClient(t, srv.URL)
	msgs := []modelrepo.Message{
		{Role: "user", Content: "transcribe", Audio: []modelrepo.AudioPart{{Data: []byte{1, 2}, MimeType: "audio/wav"}}},
	}

	_, err := client.Chat(context.Background(), msgs)
	require.ErrorIs(t, err, modelrepo.ErrAudioNotSupported)
	require.Contains(t, err.Error(), "openai")
	require.Contains(t, err.Error(), "gpt-5")

	streamClient := &OpenAIStreamClient{openAIClient: client.openAIClient}
	_, err = streamClient.Stream(context.Background(), msgs)
	require.ErrorIs(t, err, modelrepo.ErrAudioNotSupported)
}

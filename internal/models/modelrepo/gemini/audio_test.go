package gemini

import (
	"encoding/base64"
	"encoding/json"
	"testing"

	"github.com/contenox/contenox/internal/models/modelrepo"
	"github.com/stretchr/testify/require"
)

// TestUnit_BuildGeminiRequest_AudioInputAddsInlineDataPart checks audio attachments become inlineData parts with base64 payload and mime type on the wire.
func TestUnit_BuildGeminiRequest_AudioInputAddsInlineDataPart(t *testing.T) {
	t.Parallel()

	mp3Bytes := []byte("fake-mp3-bytes")
	wantB64 := base64.StdEncoding.EncodeToString(mp3Bytes)

	msgs := []modelrepo.Message{
		{
			Role:    "user",
			Content: "transcribe this recording",
			Audio:   []modelrepo.AudioPart{{Data: mp3Bytes, MimeType: "audio/mpeg"}},
		},
	}

	req, err := buildGeminiRequest("gemini-2.5-flash", msgs, nil)
	require.NoError(t, err)

	require.Len(t, req.Contents, 1)
	parts := req.Contents[0].Parts
	require.Len(t, parts, 2)
	require.Equal(t, "transcribe this recording", parts[0].Text)
	require.NotNil(t, parts[1].InlineData)
	require.Equal(t, "audio/mpeg", parts[1].InlineData.MimeType)
	require.Equal(t, mp3Bytes, parts[1].InlineData.Data)

	raw, err := json.Marshal(req)
	require.NoError(t, err)
	js := string(raw)
	require.Contains(t, js, `"inlineData":{`)
	require.Contains(t, js, `"mimeType":"audio/mpeg"`)
	require.Contains(t, js, `"data":"`+wantB64+`"`)
}

// TestUnit_BuildGeminiRequest_AudioRefusals checks the accepted media-type set and the inline size cap.
func TestUnit_BuildGeminiRequest_AudioRefusals(t *testing.T) {
	t.Parallel()

	unsupported := []modelrepo.Message{
		{Role: "user", Content: "listen", Audio: []modelrepo.AudioPart{{Data: []byte{1, 2}, MimeType: "audio/webm"}}},
	}
	_, err := buildGeminiRequest("gemini-2.5-flash", unsupported, nil)
	require.ErrorIs(t, err, modelrepo.ErrUnsupportedAudioMime)

	tooLarge := []modelrepo.Message{
		{Role: "user", Content: "listen", Audio: []modelrepo.AudioPart{
			{Data: make([]byte, modelrepo.MaxInlineAudioBytes+1), MimeType: "audio/ogg"},
		}},
	}
	_, err = buildGeminiRequest("gemini-2.5-flash", tooLarge, nil)
	require.ErrorIs(t, err, modelrepo.ErrAudioTooLarge)
}

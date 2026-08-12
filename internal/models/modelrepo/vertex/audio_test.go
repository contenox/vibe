package vertex

import (
	"encoding/base64"
	"encoding/json"
	"testing"

	"github.com/contenox/contenox/internal/models/modelrepo"
	"github.com/stretchr/testify/require"
)

// TestUnit_BuildVertexRequest_AudioInputAddsInlineDataPart checks audio attachments become inlineData parts with base64 payload and mime type on the wire.
func TestUnit_BuildVertexRequest_AudioInputAddsInlineDataPart(t *testing.T) {
	t.Parallel()

	wavBytes := []byte("fake-wav-bytes")
	wantB64 := base64.StdEncoding.EncodeToString(wavBytes)

	msgs := []modelrepo.Message{
		{
			Role:    "user",
			Content: "transcribe this recording",
			Audio:   []modelrepo.AudioPart{{Data: wavBytes, MimeType: "audio/wav"}},
		},
	}

	req, err := buildVertexRequest("gemini-2.5-flash", msgs, nil)
	require.NoError(t, err)

	require.Len(t, req.Contents, 1)
	parts := req.Contents[0].Parts
	require.Len(t, parts, 2)
	require.Equal(t, "transcribe this recording", parts[0].Text)
	require.NotNil(t, parts[1].InlineData)
	require.Equal(t, "audio/wav", parts[1].InlineData.MimeType)
	require.Equal(t, wavBytes, parts[1].InlineData.Data)

	raw, err := json.Marshal(req)
	require.NoError(t, err)
	js := string(raw)
	require.Contains(t, js, `"inlineData":{`)
	require.Contains(t, js, `"mimeType":"audio/wav"`)
	require.Contains(t, js, `"data":"`+wantB64+`"`)

	textReq, err := buildVertexRequest("gemini-2.5-flash", []modelrepo.Message{{Role: "user", Content: "hi"}}, nil)
	require.NoError(t, err)
	require.Len(t, textReq.Contents, 1)
	require.Len(t, textReq.Contents[0].Parts, 1)
	textRaw, err := json.Marshal(textReq)
	require.NoError(t, err)
	require.NotContains(t, string(textRaw), "inlineData")
}

// TestUnit_BuildVertexRequest_AudioOrderedAfterImages pins part ordering: text, then images, then audio.
func TestUnit_BuildVertexRequest_AudioOrderedAfterImages(t *testing.T) {
	t.Parallel()

	msgs := []modelrepo.Message{
		{
			Role:    "user",
			Content: "describe both",
			Images:  []modelrepo.ImagePart{{Data: []byte("img"), MimeType: "image/png"}},
			Audio:   []modelrepo.AudioPart{{Data: []byte("aud"), MimeType: "audio/mpeg"}},
		},
	}

	req, err := buildVertexRequest("gemini-2.5-pro", msgs, nil)
	require.NoError(t, err)
	require.Len(t, req.Contents, 1)
	parts := req.Contents[0].Parts
	require.Len(t, parts, 3)
	require.Equal(t, "describe both", parts[0].Text)
	require.Equal(t, "image/png", parts[1].InlineData.MimeType)
	require.Equal(t, "audio/mpeg", parts[2].InlineData.MimeType)
}

// TestUnit_BuildVertexRequest_AudioRefusals checks the accepted media-type set and the inline size cap.
func TestUnit_BuildVertexRequest_AudioRefusals(t *testing.T) {
	t.Parallel()

	unsupported := []modelrepo.Message{
		{Role: "user", Content: "listen", Audio: []modelrepo.AudioPart{{Data: []byte{1, 2}, MimeType: "audio/aac"}}},
	}
	_, err := buildVertexRequest("gemini-2.5-flash", unsupported, nil)
	require.ErrorIs(t, err, modelrepo.ErrUnsupportedAudioMime)

	tooLarge := []modelrepo.Message{
		{Role: "user", Content: "listen", Audio: []modelrepo.AudioPart{
			{Data: make([]byte, modelrepo.MaxInlineAudioBytes+1), MimeType: "audio/flac"},
		}},
	}
	_, err = buildVertexRequest("gemini-2.5-flash", tooLarge, nil)
	require.ErrorIs(t, err, modelrepo.ErrAudioTooLarge)
}

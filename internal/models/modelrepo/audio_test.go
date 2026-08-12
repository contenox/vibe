package modelrepo_test

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/contenox/contenox/internal/models/modelrepo"
	"github.com/stretchr/testify/require"
)

// TestUnit_AudioPart_JSONRoundTrip checks audio bytes ride JSON as base64 beside media type and decode back unchanged.
func TestUnit_AudioPart_JSONRoundTrip(t *testing.T) {
	t.Parallel()

	wav := []byte{'R', 'I', 'F', 'F', 0x24, 0x00, 0x00, 0x00, 'W', 'A', 'V', 'E'}
	msg := modelrepo.Message{
		Role:    "user",
		Content: "transcribe this",
		Audio:   []modelrepo.AudioPart{{Data: wav, MimeType: "audio/wav"}},
	}

	raw, err := json.Marshal(msg)
	require.NoError(t, err)
	js := string(raw)
	require.Contains(t, js, `"audio":[`)
	require.Contains(t, js, `"mime_type":"audio/wav"`)
	require.Contains(t, js, `"data":"`+base64.StdEncoding.EncodeToString(wav)+`"`)

	var back modelrepo.Message
	require.NoError(t, json.Unmarshal(raw, &back))
	require.Len(t, back.Audio, 1)
	require.Equal(t, wav, back.Audio[0].Data)
	require.Equal(t, "audio/wav", back.Audio[0].MimeType)

	textRaw, err := json.Marshal(modelrepo.Message{Role: "user", Content: "hi"})
	require.NoError(t, err)
	require.NotContains(t, string(textRaw), "audio")
}

func TestUnit_MessagesHaveAudio(t *testing.T) {
	t.Parallel()

	require.False(t, modelrepo.MessagesHaveAudio(nil))
	require.False(t, modelrepo.MessagesHaveAudio([]modelrepo.Message{{Role: "user", Content: "hi"}}))
	require.True(t, modelrepo.MessagesHaveAudio([]modelrepo.Message{
		{Role: "user", Content: "hi"},
		{Role: "user", Audio: []modelrepo.AudioPart{{Data: []byte{1}, MimeType: "audio/wav"}}},
	}))
}

// TestUnit_ValidateAudioParts_MimeAllowlist covers the accepted v1 media-type
// set and the refusal of anything outside it.
func TestUnit_ValidateAudioParts_MimeAllowlist(t *testing.T) {
	t.Parallel()

	for _, mime := range []string{"audio/wav", "audio/mpeg", "audio/mp4", "audio/ogg", "audio/flac"} {
		msgs := []modelrepo.Message{{Role: "user", Audio: []modelrepo.AudioPart{{Data: []byte{1, 2}, MimeType: mime}}}}
		require.NoError(t, modelrepo.ValidateAudioParts("gemini", "gemini-2.5-flash", msgs), "mime=%s", mime)
	}

	for _, mime := range []string{"audio/x-wav", "audio/aac", "video/mp4", "", "AUDIO/WAV"} {
		msgs := []modelrepo.Message{{Role: "user", Audio: []modelrepo.AudioPart{{Data: []byte{1, 2}, MimeType: mime}}}}
		err := modelrepo.ValidateAudioParts("gemini", "gemini-2.5-flash", msgs)
		require.ErrorIs(t, err, modelrepo.ErrUnsupportedAudioMime, "mime=%s", mime)
		require.Contains(t, err.Error(), "gemini", "the refusal names the provider")
	}

	require.NoError(t, modelrepo.ValidateAudioParts("gemini", "gemini-2.5-flash", []modelrepo.Message{{Role: "user", Content: "hi"}}))
}

// TestUnit_ValidateAudioParts_SizeCap checks total raw audio bytes across all attachments must stay within MaxInlineAudioBytes.
func TestUnit_ValidateAudioParts_SizeCap(t *testing.T) {
	t.Parallel()

	atLimit := []modelrepo.Message{{Role: "user", Audio: []modelrepo.AudioPart{
		{Data: make([]byte, modelrepo.MaxInlineAudioBytes), MimeType: "audio/wav"},
	}}}
	require.NoError(t, modelrepo.ValidateAudioParts("vertex", "gemini-2.5-pro", atLimit))

	overLimit := []modelrepo.Message{{Role: "user", Audio: []modelrepo.AudioPart{
		{Data: make([]byte, modelrepo.MaxInlineAudioBytes+1), MimeType: "audio/wav"},
	}}}
	err := modelrepo.ValidateAudioParts("vertex", "gemini-2.5-pro", overLimit)
	require.ErrorIs(t, err, modelrepo.ErrAudioTooLarge)
	require.Contains(t, err.Error(), "vertex", "the refusal names the provider")

	// The cap is per request, not per attachment.
	half := modelrepo.MaxInlineAudioBytes/2 + 1
	summed := []modelrepo.Message{
		{Role: "user", Audio: []modelrepo.AudioPart{{Data: make([]byte, half), MimeType: "audio/wav"}}},
		{Role: "user", Audio: []modelrepo.AudioPart{{Data: make([]byte, half), MimeType: "audio/flac"}}},
	}
	require.ErrorIs(t, modelrepo.ValidateAudioParts("vertex", "gemini-2.5-pro", summed), modelrepo.ErrAudioTooLarge)
}

// TestUnit_RefuseAudioInput checks a provider without audio encoding refuses audio-bearing messages, naming provider, model, and missing capability.
func TestUnit_RefuseAudioInput(t *testing.T) {
	t.Parallel()

	require.NoError(t, modelrepo.RefuseAudioInput("openai", "gpt-5", []modelrepo.Message{{Role: "user", Content: "hi"}}))

	msgs := []modelrepo.Message{{Role: "user", Content: "listen", Audio: []modelrepo.AudioPart{{Data: []byte{1}, MimeType: "audio/wav"}}}}
	err := modelrepo.RefuseAudioInput("openai", "gpt-5", msgs)
	require.Error(t, err)
	require.True(t, errors.Is(err, modelrepo.ErrAudioNotSupported))
	require.Contains(t, err.Error(), "openai", "the refusal names the provider")
	require.Contains(t, err.Error(), "gpt-5", "the refusal names the model")
	require.True(t, strings.Contains(err.Error(), "audio"), "the refusal names the capability")
}

// TestUnit_GeminiModelSupportsAudio pins the hand-maintained Google audio allowlist across mainline and excluded model families.
func TestUnit_GeminiModelSupportsAudio(t *testing.T) {
	t.Parallel()

	for _, name := range []string{
		"gemini-2.5-flash",
		"gemini-3.1-pro-preview",
		"models/gemini-2.0-flash",
		"publishers/google/models/gemini-1.5-pro",
	} {
		require.True(t, modelrepo.GeminiModelSupportsAudio(name), "model=%s", name)
	}

	for _, name := range []string{
		"gemini-2.5-flash-tts",
		"gemini-embedding-001",
		"gemini-pro-vision",
		"imagen-3.0",
		"veo-2.0",
		"qwen3:8b",
	} {
		require.False(t, modelrepo.GeminiModelSupportsAudio(name), "model=%s", name)
	}
}

package anthropic

import (
	"encoding/base64"
	"encoding/json"
	"testing"

	"github.com/contenox/beam/internal/models/modelrepo"
	msgcodec "github.com/contenox/beam/internal/models/modelrepo/codec/messages"
	"github.com/stretchr/testify/require"
)

// wireBody mirrors the subset of the Anthropic Messages request body relevant to
// content-block serialization. Source is a pointer so its absence (text-only
// blocks) is distinguishable from an emitted image source.
type wireBody struct {
	Messages []struct {
		Role    string `json:"role"`
		Content []struct {
			Type   string `json:"type"`
			Text   string `json:"text"`
			Source *struct {
				Type      string `json:"type"`
				MediaType string `json:"media_type"`
				Data      string `json:"data"`
			} `json:"source"`
		} `json:"content"`
	} `json:"messages"`
}

// TestUnit_AnthropicImageInput_Serialization asserts that a user message with an
// image attachment serializes to the Anthropic content-blocks array — a text
// block followed by a base64 `image` block — and that a text-only user message
// keeps its prior single-text-block shape unchanged.
func TestUnit_AnthropicImageInput_Serialization(t *testing.T) {
	// Raw bytes standing in for image data (PNG magic header); the codec must
	// base64-encode these verbatim, not the already-encoded form.
	raw := []byte{0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a, 0x01, 0x02, 0x03}

	t.Run("user message with one image emits a base64 image block", func(t *testing.T) {
		messages := []modelrepo.Message{{
			Role:    "user",
			Content: "what is in this image?",
			Images: []modelrepo.ImagePart{
				{Data: raw, MimeType: "image/png"},
			},
		}}

		req, _ := msgcodec.Build(messages, nil)
		b, err := json.Marshal(req)
		require.NoError(t, err)

		var body wireBody
		require.NoError(t, json.Unmarshal(b, &body))
		require.Len(t, body.Messages, 1)
		require.Equal(t, "user", body.Messages[0].Role)

		blocks := body.Messages[0].Content
		require.Len(t, blocks, 2, "text block followed by exactly one image block")

		require.Equal(t, "text", blocks[0].Type)
		require.Equal(t, "what is in this image?", blocks[0].Text)
		require.Nil(t, blocks[0].Source, "text block must not carry a source object")

		require.Equal(t, "image", blocks[1].Type)
		require.NotNil(t, blocks[1].Source)
		require.Equal(t, "base64", blocks[1].Source.Type)
		require.Equal(t, "image/png", blocks[1].Source.MediaType)
		require.Equal(t, base64.StdEncoding.EncodeToString(raw), blocks[1].Source.Data,
			"data must be the base64 of the raw image bytes")
	})

	t.Run("text-only user message keeps its prior single-text-block shape", func(t *testing.T) {
		messages := []modelrepo.Message{{Role: "user", Content: "hello"}}

		req, _ := msgcodec.Build(messages, nil)
		b, err := json.Marshal(req)
		require.NoError(t, err)

		var body wireBody
		require.NoError(t, json.Unmarshal(b, &body))
		require.Len(t, body.Messages, 1)

		blocks := body.Messages[0].Content
		require.Len(t, blocks, 1, "text-only user message stays a single text block")
		require.Equal(t, "text", blocks[0].Type)
		require.Equal(t, "hello", blocks[0].Text)
		require.Nil(t, blocks[0].Source, "text-only block must omit the source object")
	})
}

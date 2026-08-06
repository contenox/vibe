package acpsvc

import (
	"encoding/base64"
	"testing"

	"github.com/contenox/contenox/libacp"
	"github.com/stretchr/testify/require"
)

func TestUnit_ExtractImageParts_SplitsImagesFromText(t *testing.T) {
	png := []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a}
	blocks := []libacp.ContentBlock{
		libacp.NewTextContent("what is this?"),
		libacp.NewImageContent(base64.StdEncoding.EncodeToString(png), "image/png"),
		libacp.NewTextContent("second line"),
	}

	images, rest := extractImageParts(blocks)

	require.Len(t, images, 1)
	require.Equal(t, png, images[0].Data)
	require.Equal(t, "image/png", images[0].MimeType)
	// Text blocks pass through untouched and in order, so FlattenContent's
	// projection of the remaining blocks is unchanged by the extraction.
	require.Len(t, rest, 2)
	text, dropped := libacp.FlattenContent(rest)
	require.Equal(t, "what is this?\nsecond line", text)
	require.Empty(t, dropped)
}

func TestUnit_ExtractImageParts_InvalidBase64StaysDroppedVisible(t *testing.T) {
	blocks := []libacp.ContentBlock{
		libacp.NewImageContent("not-base64!!", "image/png"),
	}

	images, rest := extractImageParts(blocks)

	require.Empty(t, images)
	// The broken block flows on to FlattenContent, which reports it dropped —
	// a visible degradation, never a silent one.
	require.Len(t, rest, 1)
	_, dropped := libacp.FlattenContent(rest)
	require.Equal(t, []string{string(libacp.ContentKindImage)}, dropped)
}

func TestUnit_ExtractImageParts_ImageOnlyPromptYieldsNoText(t *testing.T) {
	blocks := []libacp.ContentBlock{
		libacp.NewImageContent(base64.StdEncoding.EncodeToString([]byte{1, 2, 3}), "image/jpeg"),
	}

	images, rest := extractImageParts(blocks)

	require.Len(t, images, 1)
	require.Empty(t, rest)
}

package contenoxcli

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// minimalPNG is the 8-byte PNG signature plus padding — enough for
// http.DetectContentType to sniff image/png.
var minimalPNG = []byte{0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A, 0, 0, 0, 0}

func TestUnit_LoadImageAttachments_ReadsAndSniffsImages(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "shot.png")
	require.NoError(t, os.WriteFile(path, minimalPNG, 0o644))

	images, err := loadImageAttachments([]string{path})
	require.NoError(t, err)
	require.Len(t, images, 1)
	require.Equal(t, "image/png", images[0].MimeType)
	require.Equal(t, minimalPNG, images[0].Data)
}

func TestUnit_LoadImageAttachments_RejectsNonImage(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "notes.txt")
	require.NoError(t, os.WriteFile(path, []byte("plain text, not pixels"), 0o644))

	_, err := loadImageAttachments([]string{path})
	require.Error(t, err)
	require.Contains(t, err.Error(), "not an image")
}

func TestUnit_LoadImageAttachments_MissingFileNamesThePath(t *testing.T) {
	_, err := loadImageAttachments([]string{"/no/such/picture.png"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "/no/such/picture.png")
}

func TestUnit_LoadImageAttachments_EmptyIsNil(t *testing.T) {
	images, err := loadImageAttachments(nil)
	require.NoError(t, err)
	require.Nil(t, images)
}

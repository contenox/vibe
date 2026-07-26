package contenoxcli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/contenox/beam/internal/models/hostcapacity"
	"github.com/contenox/beam/internal/models/modelregistry"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestUnit_PrintModelRegistryListIncludesBackendColumn(t *testing.T) {
	var out bytes.Buffer
	entries := []modelregistry.ModelDescriptor{
		{Name: "qwen3-8b-ov", Family: "qwen3-ov", Backend: "openvino", SourceURL: "https://example.com/ov", SizeBytes: 2 * 1024 * 1024, Curated: true, Notes: "ov", UseCase: "chat", RecommendedVRAMGB: 8},
		{Name: "qwen3-8b", Family: "qwen3", SourceURL: "https://example.com/gguf", SizeBytes: 1024 * 1024, Curated: true, Notes: "gguf", UseCase: "coding", RecommendedVRAMGB: 6},
	}

	require.NoError(t, printModelRegistryList(&out, entries, hostcapacity.Budget{
		Known:      true,
		Kind:       "gpu",
		Label:      "test GPU",
		TotalBytes: 8 << 30,
		FreeBytes:  8 << 30,
	}))

	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	require.Len(t, lines, 3)
	assert.Contains(t, lines[0], "BACKEND")
	assert.Contains(t, lines[0], "FAMILY")
	assert.Contains(t, lines[0], "VRAM")
	assert.Contains(t, lines[0], "USE")
	assert.Contains(t, lines[0], "FITS")
	assert.Contains(t, lines[0], "NOTES")
	assert.Contains(t, lines[1], "qwen3-8b")
	assert.Contains(t, lines[1], "qwen3")
	assert.Contains(t, lines[1], "llama")
	assert.Contains(t, lines[1], "6GB")
	assert.Contains(t, lines[1], "coding")
	assert.Contains(t, lines[1], "yes")
	assert.Contains(t, lines[1], "gguf")
	assert.Contains(t, lines[2], "qwen3-8b-ov")
	assert.Contains(t, lines[2], "qwen3-ov")
	assert.Contains(t, lines[2], "openvino")
	assert.Contains(t, lines[2], "8GB")
	assert.Contains(t, lines[2], "chat")
	assert.Contains(t, lines[2], "ov")
}

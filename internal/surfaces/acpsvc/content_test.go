package acpsvc

import (
	"testing"

	"github.com/contenox/contenox/libacp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestUnit_FlattenPromptBlocks_ResourceLinkFromMention pins: a resource_link block (an @-mention) becomes a "name: uri" line, never dropped.
func TestUnit_FlattenPromptBlocks_ResourceLinkFromMention(t *testing.T) {
	blocks := []libacp.ContentBlock{
		{Type: string(libacp.ContentKindText), Text: "review @src/main.go"},
		{Type: string(libacp.ContentKindResourceLink), Name: "src/main.go", URI: "src/main.go"},
	}
	out, dropped := libacp.FlattenContent(blocks)
	assert.Empty(t, dropped, "a resource_link with a name and uri must not be dropped")
	assert.Equal(t, "review @src/main.go\nsrc/main.go: src/main.go", out,
		"the mention must reach the agent as a resolvable name: uri reference line")
}

// TestUnit_FlattenPromptBlocks_ResourceLinkUriOnly pins: a resource_link with no name still surfaces its uri, not a drop.
func TestUnit_FlattenPromptBlocks_ResourceLinkUriOnly(t *testing.T) {
	out, dropped := libacp.FlattenContent([]libacp.ContentBlock{
		{Type: string(libacp.ContentKindResourceLink), URI: "notes.md"},
	})
	require.Empty(t, dropped)
	assert.Equal(t, "notes.md", out)
}

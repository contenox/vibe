package acpsvc

import (
	"testing"

	"github.com/contenox/beam/libacp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestUnit_FlattenPromptBlocks_ResourceLinkFromMention pins the server side of
// the @-mention wire contract: beam's composer emits a text block plus one
// resource_link block per mention ({type:"resource_link", name:<path>,
// uri:<path>} — see packages/beam/.../mentions.ts promptBlocksFromDraft), and
// the runtime must consume the resource_link as a "name: uri" line appended to
// the prompt (the reference the agent follows with local_fs), never dropping it.
// Reference only: no embedded resource content is required for a mention.
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

// TestUnit_FlattenPromptBlocks_ResourceLinkUriOnly covers the degenerate mention
// shape (uri without a distinct name): it still surfaces the uri, not a drop.
func TestUnit_FlattenPromptBlocks_ResourceLinkUriOnly(t *testing.T) {
	out, dropped := libacp.FlattenContent([]libacp.ContentBlock{
		{Type: string(libacp.ContentKindResourceLink), URI: "notes.md"},
	})
	require.Empty(t, dropped)
	assert.Equal(t, "notes.md", out)
}

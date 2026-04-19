package local

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/contenox/contenox/runtime/internal/modelrepo"
)

func TestUnit_LocalProvider_Capabilities(t *testing.T) {
	p := &localProvider{name: "test", modelDir: "/fake", caps: modelrepo.CapabilityConfig{ContextLength: 4096}}
	assert.True(t, p.CanChat())
	assert.True(t, p.CanPrompt())
	assert.True(t, p.CanStream())
	assert.True(t, p.CanEmbed())
	assert.False(t, p.CanThink())
	assert.Equal(t, "local", p.GetType())
	assert.Equal(t, "test", p.ModelName())
	assert.Equal(t, "local:test", p.GetID())
	assert.Equal(t, []string{"local"}, p.GetBackendIDs())
	assert.Equal(t, 4096, p.GetContextLength())
}

func TestUnit_LocalProvider_AllConnectionsReturnNonNil(t *testing.T) {
	p := &localProvider{name: "test", modelDir: "/fake"}

	chat, err := p.GetChatConnection(context.Background(), "local")
	require.NoError(t, err)
	assert.NotNil(t, chat)

	prompt, err := p.GetPromptConnection(context.Background(), "local")
	require.NoError(t, err)
	assert.NotNil(t, prompt)

	stream, err := p.GetStreamConnection(context.Background(), "local")
	require.NoError(t, err)
	assert.NotNil(t, stream)

	embed, err := p.GetEmbedConnection(context.Background(), "local")
	require.NoError(t, err)
	assert.NotNil(t, embed)
}

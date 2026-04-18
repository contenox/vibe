package local

import (
	"context"
	"path/filepath"

	"github.com/contenox/contenox/internal/modelrepo"
)

type localProvider struct {
	name    string
	modelDir string
	caps    modelrepo.CapabilityConfig
}

func newLocalProvider(name, modelDir string, caps modelrepo.CapabilityConfig) modelrepo.Provider {
	return &localProvider{name: name, modelDir: modelDir, caps: caps}
}

func (p *localProvider) GetBackendIDs() []string  { return []string{"local"} }
func (p *localProvider) ModelName() string         { return p.name }
func (p *localProvider) GetID() string             { return "local:" + p.name }
func (p *localProvider) GetType() string           { return "local" }
func (p *localProvider) GetContextLength() int     { return p.caps.ContextLength }
func (p *localProvider) CanChat() bool             { return true }
func (p *localProvider) CanEmbed() bool            { return true }
func (p *localProvider) CanStream() bool           { return true }
func (p *localProvider) CanPrompt() bool           { return true }
func (p *localProvider) CanThink() bool            { return false }

func (p *localProvider) GetChatConnection(_ context.Context, _ string) (modelrepo.LLMChatClient, error) {
	modelPath := filepath.Join(p.modelDir, p.name, "model.gguf")
	return &localChatClient{modelPath: modelPath}, nil
}

func (p *localProvider) GetPromptConnection(_ context.Context, _ string) (modelrepo.LLMPromptExecClient, error) {
	modelPath := filepath.Join(p.modelDir, p.name, "model.gguf")
	return &localPromptClient{modelPath: modelPath}, nil
}

func (p *localProvider) GetEmbedConnection(_ context.Context, _ string) (modelrepo.LLMEmbedClient, error) {
	modelPath := filepath.Join(p.modelDir, p.name, "model.gguf")
	return &localEmbedClient{modelPath: modelPath}, nil
}

func (p *localProvider) GetStreamConnection(_ context.Context, _ string) (modelrepo.LLMStreamClient, error) {
	modelPath := filepath.Join(p.modelDir, p.name, "model.gguf")
	return &localStreamClient{modelPath: modelPath}, nil
}

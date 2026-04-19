package modelrepo

import "context"

type Provider interface {
	GetBackendIDs() []string
	ModelName() string
	GetID() string
	GetType() string
	GetContextLength() int
	CanChat() bool
	CanEmbed() bool
	CanStream() bool
	CanPrompt() bool
	CanThink() bool
	GetChatConnection(ctx context.Context, backendID string) (LLMChatClient, error)
	GetPromptConnection(ctx context.Context, backendID string) (LLMPromptExecClient, error)
	GetEmbedConnection(ctx context.Context, backendID string) (LLMEmbedClient, error)
	GetStreamConnection(ctx context.Context, backendID string) (LLMStreamClient, error)
}

type CapabilityConfig struct {
	ContextLength int
	CanChat       bool
	CanEmbed      bool
	CanStream     bool
	CanPrompt     bool
	CanThink      bool
}

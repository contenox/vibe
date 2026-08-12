package modelrepo

import "context"

type Provider interface {
	GetBackendIDs() []string
	ModelName() string
	GetID() string
	GetType() string
	GetContextLength() int
	// GetMaxOutputTokens returns the provider's hard ceiling on output tokens; 0 means unknown or unlimited.
	GetMaxOutputTokens() int
	CanChat() bool
	CanEmbed() bool
	CanStream() bool
	CanPrompt() bool
	CanThink() bool
	// CanVision reports whether the model accepts image input.
	CanVision() bool
	// CanAudio reports whether the model accepts audio input.
	CanAudio() bool
	GetChatConnection(ctx context.Context, backendID string) (LLMChatClient, error)
	GetPromptConnection(ctx context.Context, backendID string) (LLMPromptExecClient, error)
	GetEmbedConnection(ctx context.Context, backendID string) (LLMEmbedClient, error)
	GetStreamConnection(ctx context.Context, backendID string) (LLMStreamClient, error)
}

type CapabilityConfig struct {
	ContextLength int
	// MaxOutputTokens is the provider's hard ceiling on output tokens; 0 means unknown, no clamp applied.
	MaxOutputTokens int
	CanChat         bool
	CanEmbed        bool
	CanStream       bool
	CanPrompt       bool
	CanThink        bool
	// CanVision marks models that accept image input.
	CanVision bool
	// CanAudio marks models that accept audio input.
	CanAudio bool
}

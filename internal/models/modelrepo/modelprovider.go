package modelrepo

import "context"

type Provider interface {
	GetBackendIDs() []string
	ModelName() string
	GetID() string
	GetType() string
	GetContextLength() int
	// GetMaxOutputTokens returns the provider's hard ceiling on output tokens
	// (maxOutputTokens / max_tokens / max_completion_tokens in the wire format).
	// Returns 0 when the ceiling is unknown or effectively unlimited.
	GetMaxOutputTokens() int
	CanChat() bool
	CanEmbed() bool
	CanStream() bool
	CanPrompt() bool
	CanThink() bool
	// CanVision reports whether the model accepts image input. The resolver
	// only routes image-bearing requests to providers that return true.
	CanVision() bool
	GetChatConnection(ctx context.Context, backendID string) (LLMChatClient, error)
	GetPromptConnection(ctx context.Context, backendID string) (LLMPromptExecClient, error)
	GetEmbedConnection(ctx context.Context, backendID string) (LLMEmbedClient, error)
	GetStreamConnection(ctx context.Context, backendID string) (LLMStreamClient, error)
}

type CapabilityConfig struct {
	ContextLength int
	// MaxOutputTokens is the provider's hard ceiling on output tokens.
	// Leave as 0 when unknown; the client will not clamp.
	MaxOutputTokens int
	CanChat         bool
	CanEmbed        bool
	CanStream       bool
	CanPrompt       bool
	CanThink        bool
	// CanVision marks models that accept image input.
	CanVision bool
}

package llmresolver

import (
	"context"

	"github.com/contenox/vibe/internal/modelrepo"
	"github.com/contenox/vibe/libtracker"
)

// Resolver is the interface for selecting appropriate model providers based on requirements.
//
// The resolver handles the process of:
// 1. Finding available providers that match requirements
// 2. Selecting the best provider/backend combination
// 3. Establishing a connection to the selected backend
type Resolver interface {
	// ResolveChat selects a provider capable of handling chat requests
	// and returns a connected client for that provider.
	//
	// Parameters:
	//   ctx: Context for cancellation and timeouts
	//   req: Requirements for the model (provider types, model names, context length)
	//   getProviders: Function to fetch available providers (decoupled from storage)
	//   policy: Selection strategy for choosing among candidates
	//
	// Returns:
	//   client: Ready-to-use chat client
	//   provider: The selected provider metadata
	//   backendID: Identifier for the specific backend instance
	//   error: Any error encountered during resolution
	ResolveChat(
		ctx context.Context,
		req Request) (modelrepo.LLMChatClient, modelrepo.Provider, string, error)

	// ResolvePromptExecute selects a provider capable of executing prompt-based requests
	// and returns a connected client for that provider.
	//
	// See ResolveChat for parameter and return details.
	ResolvePromptExecute(
		ctx context.Context,
		req Request,
	) (modelrepo.LLMPromptExecClient, modelrepo.Provider, string, error)

	// ResolveEmbed selects a provider capable of generating embeddings
	// and returns a connected client for that provider.
	//
	// Parameters:
	//   ctx: Context for cancellation and timeouts
	//   req: Requirements for the embedding model
	//   getProviders: Function to fetch available providers
	//   policy: Selection strategy for choosing among candidates
	//
	// Returns:
	//   client: Ready-to-use embedding client
	//   provider: The selected provider metadata
	//   backendID: Identifier for the specific backend instance
	//   error: Any error encountered during resolution
	ResolveEmbed(
		ctx context.Context,
		req EmbedRequest,
	) (modelrepo.LLMEmbedClient, modelrepo.Provider, string, error)

	// ResolveStream selects a provider capable of streaming responses
	// and returns a connected client for that provider.
	//
	// See ResolveEmbed for parameter and return details.
	ResolveStream(
		ctx context.Context,
		req Request,
	) (modelrepo.LLMStreamClient, modelrepo.Provider, string, error)
}

// Request contains requirements for selecting a model provider.
type Request struct {
	// ProviderTypes specifies which provider implementations to consider.
	// If empty, all available providers are considered.
	ProviderTypes []string

	// ModelNames specifies preferred model names in priority order.
	// The resolver will try these models first before considering others.
	// If empty, any model is acceptable.
	ModelNames []string

	// ContextLength specifies the minimum required context window length.
	// Providers with smaller context windows will be excluded.
	// If 0, no minimum is enforced.
	ContextLength int

	// Tracker is used for activity monitoring and tracing.
	// While not serializable, it's preserved through resolution chains.
	Tracker libtracker.ActivityTracker
}

// EmbedRequest is a specialized request for embedding operations.
type EmbedRequest struct {
	// ModelName specifies the preferred embedding model.
	// If empty, a default model will be selected.
	ModelName string

	// ProviderType specifies which provider implementation to use.
	// If empty, any provider is acceptable.
	ProviderType string

	// Tracker is used for activity monitoring and tracing.
	Tracker libtracker.ActivityTracker
}

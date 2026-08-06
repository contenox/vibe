package anthropic

import (
	"context"
	"fmt"
	"net/http"

	"github.com/contenox/contenox/internal/models/modelrepo"
	"github.com/contenox/contenox/libtracker"
)

type anthropicProvider struct {
	id              string
	apiKey          string
	modelName       string
	baseURL         string
	httpClient      *http.Client
	contextLength   int
	maxOutputTokens int
	canChat         bool
	canPrompt       bool
	canStream       bool
	canThink        bool
	canVision       bool
	tracker         libtracker.ActivityTracker
}

// NewAnthropicProvider returns a modelrepo.Provider for the direct Anthropic API.
func NewAnthropicProvider(apiKey, modelName string, backendURLs []string, capability modelrepo.CapabilityConfig, httpClient *http.Client, tracker libtracker.ActivityTracker) modelrepo.Provider {
	if httpClient == nil {
		httpClient = modelrepo.SharedHTTPClient
	}
	if tracker == nil {
		tracker = libtracker.NoopTracker{}
	}
	baseURL := defaultBaseURL
	if len(backendURLs) > 0 && backendURLs[0] != "" {
		baseURL = backendURLs[0]
	}
	return &anthropicProvider{
		id:              fmt.Sprintf("anthropic-%s", modelName),
		apiKey:          apiKey,
		modelName:       modelName,
		baseURL:         baseURL,
		httpClient:      httpClient,
		contextLength:   capability.ContextLength,
		maxOutputTokens: capability.MaxOutputTokens,
		canChat:         capability.CanChat,
		canPrompt:       capability.CanPrompt,
		canStream:       capability.CanStream,
		canThink:        capability.CanThink,
		canVision:       capability.CanVision,
		tracker:         tracker,
	}
}

func (p *anthropicProvider) GetBackendIDs() []string { return []string{p.baseURL} }
func (p *anthropicProvider) ModelName() string       { return p.modelName }
func (p *anthropicProvider) GetID() string           { return p.id }
func (p *anthropicProvider) GetType() string         { return "anthropic" }
func (p *anthropicProvider) GetContextLength() int   { return p.contextLength }
func (p *anthropicProvider) GetMaxOutputTokens() int { return p.maxOutputTokens }
func (p *anthropicProvider) CanChat() bool           { return p.canChat }
func (p *anthropicProvider) CanEmbed() bool          { return false }
func (p *anthropicProvider) CanStream() bool         { return p.canStream }
func (p *anthropicProvider) CanPrompt() bool         { return p.canPrompt }
func (p *anthropicProvider) CanThink() bool          { return p.canThink }
func (p *anthropicProvider) CanVision() bool         { return p.canVision }

func (p *anthropicProvider) base() anthropicClient {
	return anthropicClient{
		baseURL:         p.baseURL,
		apiKey:          p.apiKey,
		modelName:       p.modelName,
		httpClient:      p.httpClient,
		canThink:        p.canThink,
		maxOutputTokens: p.maxOutputTokens,
		tracker:         p.tracker,
	}
}

func (p *anthropicProvider) GetChatConnection(_ context.Context, _ string) (modelrepo.LLMChatClient, error) {
	if !p.CanChat() {
		return nil, fmt.Errorf("model %s does not support chat interactions", p.modelName)
	}
	return &anthropicChatClient{anthropicClient: p.base()}, nil
}

func (p *anthropicProvider) GetStreamConnection(_ context.Context, _ string) (modelrepo.LLMStreamClient, error) {
	if !p.CanStream() {
		return nil, fmt.Errorf("model %s does not support streaming interactions", p.modelName)
	}
	return &anthropicStreamClient{anthropicClient: p.base()}, nil
}

func (p *anthropicProvider) GetPromptConnection(_ context.Context, _ string) (modelrepo.LLMPromptExecClient, error) {
	if !p.CanPrompt() {
		return nil, fmt.Errorf("model %s does not support prompt interactions", p.modelName)
	}
	return &anthropicPromptClient{anthropicClient: p.base()}, nil
}

func (p *anthropicProvider) GetEmbedConnection(_ context.Context, _ string) (modelrepo.LLMEmbedClient, error) {
	return nil, fmt.Errorf("model %s (anthropic) does not support embeddings", p.modelName)
}

var _ modelrepo.Provider = (*anthropicProvider)(nil)

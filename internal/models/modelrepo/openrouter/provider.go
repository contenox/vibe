package openrouter

import (
	"context"
	"fmt"
	"net/http"

	"github.com/contenox/beam/internal/libtracker"
	"github.com/contenox/beam/internal/models/modelrepo"
)

type orProvider struct {
	id              string
	apiKey          string
	modelName       string
	baseURL         string
	httpClient      *http.Client
	contextLength   int
	maxOutputTokens int
	canChat         bool
	canPrompt       bool
	canEmbed        bool
	canStream       bool
	canThink        bool
	canVision       bool
	tracker         libtracker.ActivityTracker
}

func newOpenRouterProvider(apiKey, modelName, baseURL string, capability modelrepo.CapabilityConfig, httpClient *http.Client, tracker libtracker.ActivityTracker) modelrepo.Provider {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	if tracker == nil {
		tracker = libtracker.NoopTracker{}
	}
	if baseURL == "" {
		baseURL = defaultBaseURL
	}
	return &orProvider{
		id:              fmt.Sprintf("openrouter-%s", modelName),
		apiKey:          apiKey,
		modelName:       modelName,
		baseURL:         baseURL,
		httpClient:      httpClient,
		contextLength:   capability.ContextLength,
		maxOutputTokens: capability.MaxOutputTokens,
		canChat:         capability.CanChat,
		canPrompt:       capability.CanPrompt,
		canEmbed:        capability.CanEmbed,
		canStream:       capability.CanStream,
		canThink:        capability.CanThink,
		canVision:       capability.CanVision,
		tracker:         tracker,
	}
}

func (p *orProvider) GetBackendIDs() []string { return []string{p.baseURL} }
func (p *orProvider) ModelName() string       { return p.modelName }
func (p *orProvider) GetID() string           { return p.id }
func (p *orProvider) GetType() string         { return "openrouter" }
func (p *orProvider) GetContextLength() int   { return p.contextLength }
func (p *orProvider) GetMaxOutputTokens() int { return p.maxOutputTokens }
func (p *orProvider) CanChat() bool           { return p.canChat }
func (p *orProvider) CanEmbed() bool          { return p.canEmbed }
func (p *orProvider) CanStream() bool         { return p.canStream }
func (p *orProvider) CanPrompt() bool         { return p.canPrompt }
func (p *orProvider) CanThink() bool          { return p.canThink }
func (p *orProvider) CanVision() bool         { return p.canVision }

func (p *orProvider) base() orClient {
	return orClient{
		baseURL:         p.baseURL,
		apiKey:          p.apiKey,
		modelName:       p.modelName,
		maxOutputTokens: p.maxOutputTokens,
		httpClient:      p.httpClient,
		tracker:         p.tracker,
	}
}

func (p *orProvider) GetChatConnection(_ context.Context, _ string) (modelrepo.LLMChatClient, error) {
	if !p.CanChat() {
		return nil, fmt.Errorf("model %s does not support chat interactions", p.modelName)
	}
	return &orChatClient{orClient: p.base()}, nil
}

func (p *orProvider) GetStreamConnection(_ context.Context, _ string) (modelrepo.LLMStreamClient, error) {
	if !p.CanStream() {
		return nil, fmt.Errorf("model %s does not support streaming interactions", p.modelName)
	}
	return &orStreamClient{orClient: p.base()}, nil
}

func (p *orProvider) GetPromptConnection(_ context.Context, _ string) (modelrepo.LLMPromptExecClient, error) {
	if !p.CanPrompt() {
		return nil, fmt.Errorf("model %s does not support prompt interactions", p.modelName)
	}
	return &orPromptClient{orClient: p.base()}, nil
}

func (p *orProvider) GetEmbedConnection(_ context.Context, _ string) (modelrepo.LLMEmbedClient, error) {
	return nil, fmt.Errorf("model %s (openrouter) does not support embeddings via this provider", p.modelName)
}

var _ modelrepo.Provider = (*orProvider)(nil)

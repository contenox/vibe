package mistral

import (
	"context"
	"fmt"
	"net/http"

	"github.com/contenox/beam/internal/libtracker"
	"github.com/contenox/beam/internal/models/modelrepo"
)

type mistralProvider struct {
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

// NewMistralProvider returns a modelrepo.Provider for the direct Mistral API.
func NewMistralProvider(apiKey, modelName string, backendURLs []string, capability modelrepo.CapabilityConfig, httpClient *http.Client, tracker libtracker.ActivityTracker) modelrepo.Provider {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	if tracker == nil {
		tracker = libtracker.NoopTracker{}
	}
	baseURL := defaultBaseURL
	if len(backendURLs) > 0 && backendURLs[0] != "" {
		baseURL = backendURLs[0]
	}
	return &mistralProvider{
		id:              fmt.Sprintf("mistral-%s", modelName),
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

func (p *mistralProvider) GetBackendIDs() []string { return []string{p.baseURL} }
func (p *mistralProvider) ModelName() string       { return p.modelName }
func (p *mistralProvider) GetID() string           { return p.id }
func (p *mistralProvider) GetType() string         { return "mistral" }
func (p *mistralProvider) GetContextLength() int   { return p.contextLength }
func (p *mistralProvider) GetMaxOutputTokens() int { return p.maxOutputTokens }
func (p *mistralProvider) CanChat() bool           { return p.canChat }
func (p *mistralProvider) CanEmbed() bool          { return p.canEmbed }
func (p *mistralProvider) CanStream() bool         { return p.canStream }
func (p *mistralProvider) CanPrompt() bool         { return p.canPrompt }
func (p *mistralProvider) CanThink() bool          { return p.canThink }
func (p *mistralProvider) CanVision() bool         { return p.canVision }

func (p *mistralProvider) base() mistralClient {
	return mistralClient{
		baseURL:         p.baseURL,
		apiKey:          p.apiKey,
		modelName:       p.modelName,
		maxOutputTokens: p.maxOutputTokens,
		httpClient:      p.httpClient,
		tracker:         p.tracker,
	}
}

func (p *mistralProvider) GetChatConnection(_ context.Context, _ string) (modelrepo.LLMChatClient, error) {
	if !p.CanChat() {
		return nil, fmt.Errorf("model %s does not support chat interactions", p.modelName)
	}
	return &mistralChatClient{mistralClient: p.base()}, nil
}

func (p *mistralProvider) GetStreamConnection(_ context.Context, _ string) (modelrepo.LLMStreamClient, error) {
	if !p.CanStream() {
		return nil, fmt.Errorf("model %s does not support streaming interactions", p.modelName)
	}
	return &mistralStreamClient{mistralClient: p.base()}, nil
}

func (p *mistralProvider) GetPromptConnection(_ context.Context, _ string) (modelrepo.LLMPromptExecClient, error) {
	if !p.CanPrompt() {
		return nil, fmt.Errorf("model %s does not support prompt interactions", p.modelName)
	}
	return &mistralPromptClient{mistralClient: p.base()}, nil
}

func (p *mistralProvider) GetEmbedConnection(_ context.Context, _ string) (modelrepo.LLMEmbedClient, error) {
	return nil, fmt.Errorf("model %s (mistral) does not support embeddings via this provider", p.modelName)
}

var _ modelrepo.Provider = (*mistralProvider)(nil)

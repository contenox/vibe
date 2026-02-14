package openai

import (
	"context"
	"fmt"
	"net/http"

	"github.com/contenox/vibe/internal/modelrepo"
	"github.com/contenox/vibe/libtracker"
)

type OpenAIProvider struct {
	id            string
	apiKey        string
	modelName     string
	baseURL       string
	httpClient    *http.Client
	contextLength int
	canChat       bool
	canPrompt     bool
	canEmbed      bool
	canStream     bool
	tracker       libtracker.ActivityTracker
}

func NewOpenAIProvider(apiKey, modelName string, backendURLs []string, capability modelrepo.CapabilityConfig, httpClient *http.Client, tracker libtracker.ActivityTracker) modelrepo.Provider {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	if len(backendURLs) == 0 {
		backendURLs = []string{"https://api.openai.com/v1"}
	}

	if tracker == nil {
		tracker = libtracker.NoopTracker{}
	}

	apiBaseURL := backendURLs[0]
	id := fmt.Sprintf("openai-%s", modelName)

	return &OpenAIProvider{
		id:            id,
		apiKey:        apiKey,
		modelName:     modelName,
		baseURL:       apiBaseURL,
		httpClient:    httpClient,
		contextLength: capability.ContextLength,
		canChat:       capability.CanChat,
		canPrompt:     capability.CanPrompt,
		canEmbed:      capability.CanEmbed,
		canStream:     capability.CanStream,
		tracker:       tracker,
	}
}

func (p *OpenAIProvider) GetBackendIDs() []string {
	return []string{p.baseURL}
}

func (p *OpenAIProvider) ModelName() string {
	return p.modelName
}

func (p *OpenAIProvider) GetID() string {
	return p.id
}

func (p *OpenAIProvider) GetType() string {
	return "openai"
}

func (p *OpenAIProvider) GetContextLength() int {
	return p.contextLength
}

func (p *OpenAIProvider) CanChat() bool {
	return p.canChat
}

func (p *OpenAIProvider) CanEmbed() bool {
	return p.canEmbed
}

func (p *OpenAIProvider) CanStream() bool {
	return p.canStream
}

func (p *OpenAIProvider) CanPrompt() bool {
	return p.canPrompt
}

func (p *OpenAIProvider) CanThink() bool {
	return false
}

func (p *OpenAIProvider) GetChatConnection(ctx context.Context, backendID string) (modelrepo.LLMChatClient, error) {
	if !p.CanChat() {
		return nil, fmt.Errorf("model %s does not support chat interactions", p.modelName)
	}
	return &OpenAIChatClient{
		openAIClient: openAIClient{
			baseURL:    p.baseURL,
			apiKey:     p.apiKey,
			httpClient: p.httpClient,
			modelName:  p.modelName,
			maxTokens:  p.contextLength,
			tracker:    p.tracker,
		},
	}, nil
}

func (p *OpenAIProvider) GetPromptConnection(ctx context.Context, backendID string) (modelrepo.LLMPromptExecClient, error) {
	if !p.CanPrompt() {
		return nil, fmt.Errorf("model %s does not support prompt interactions", p.modelName)
	}
	return &OpenAIPromptClient{
		openAIClient: openAIClient{
			baseURL:    p.baseURL,
			apiKey:     p.apiKey,
			httpClient: p.httpClient,
			modelName:  p.modelName,
			maxTokens:  p.contextLength,
			tracker:    p.tracker,
		},
	}, nil
}

func (p *OpenAIProvider) GetEmbedConnection(ctx context.Context, backendID string) (modelrepo.LLMEmbedClient, error) {
	if !p.CanEmbed() {
		return nil, fmt.Errorf("model %s does not support embedding interactions", p.modelName)
	}
	return &OpenAIEmbedClient{
		openAIClient: openAIClient{
			baseURL:    p.baseURL,
			apiKey:     p.apiKey,
			httpClient: p.httpClient,
			modelName:  p.modelName,
			tracker:    p.tracker,
		},
	}, nil
}

func (p *OpenAIProvider) GetStreamConnection(ctx context.Context, backendID string) (modelrepo.LLMStreamClient, error) {
	if !p.CanStream() {
		return nil, fmt.Errorf("model %s does not support streaming interactions", p.modelName)
	}
	return &OpenAIStreamClient{
		openAIClient: openAIClient{
			baseURL:    p.baseURL,
			apiKey:     p.apiKey,
			httpClient: p.httpClient,
			modelName:  p.modelName,
			maxTokens:  p.contextLength,
			tracker:    p.tracker,
		},
	}, nil
}

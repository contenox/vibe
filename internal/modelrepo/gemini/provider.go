package gemini

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/contenox/vibe/internal/modelrepo"
	"github.com/contenox/vibe/libtracker"
)

type GeminiProvider struct {
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

func NewGeminiProvider(apiKey string, modelName string, baseURLs []string, cap modelrepo.CapabilityConfig, httpClient *http.Client, tracker libtracker.ActivityTracker) modelrepo.Provider {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	if len(baseURLs) == 0 {
		baseURLs = []string{"https://generativelanguage.googleapis.com"}
	}
	if tracker == nil {
		tracker = libtracker.NoopTracker{}
	}
	apiBaseURL := baseURLs[0]
	id := fmt.Sprintf("gemini-%s", modelName)
	modelName, _ = strings.CutPrefix(modelName, "models/")
	return &GeminiProvider{
		id:            id,
		apiKey:        apiKey,
		modelName:     modelName,
		baseURL:       apiBaseURL,
		httpClient:    httpClient,
		contextLength: cap.ContextLength,
		canChat:       cap.CanChat,
		canPrompt:     cap.CanPrompt,
		canEmbed:      cap.CanEmbed,
		canStream:     cap.CanStream,
		tracker:       tracker,
	}
}

func (p *GeminiProvider) GetBackendIDs() []string { return []string{p.baseURL} }
func (p *GeminiProvider) ModelName() string       { return p.modelName }
func (p *GeminiProvider) GetID() string           { return p.id }
func (p *GeminiProvider) GetType() string         { return "gemini" }
func (p *GeminiProvider) GetContextLength() int   { return p.contextLength }
func (p *GeminiProvider) CanChat() bool           { return p.canChat }
func (p *GeminiProvider) CanEmbed() bool          { return p.canEmbed }
func (p *GeminiProvider) CanStream() bool         { return p.canStream }
func (p *GeminiProvider) CanPrompt() bool         { return p.canPrompt }
func (p *GeminiProvider) CanThink() bool          { return false }

func (p *GeminiProvider) GetChatConnection(ctx context.Context, backendID string) (modelrepo.LLMChatClient, error) {
	if !p.CanChat() {
		return nil, fmt.Errorf("model %s does not support chat interactions", p.modelName)
	}
	return &GeminiChatClient{
		geminiClient: geminiClient{
			modelName:  p.modelName,
			baseURL:    p.baseURL,
			httpClient: p.httpClient,
			maxTokens:  p.contextLength,
			apiKey:     p.apiKey,
			tracker:    p.tracker,
		},
	}, nil
}

func (p *GeminiProvider) GetPromptConnection(ctx context.Context, backendID string) (modelrepo.LLMPromptExecClient, error) {
	if !p.CanPrompt() {
		return nil, fmt.Errorf("model %s does not support prompt interactions", p.modelName)
	}
	return &GeminiPromptClient{
		geminiClient: geminiClient{
			modelName:  p.modelName,
			baseURL:    p.baseURL,
			httpClient: p.httpClient,
			maxTokens:  p.contextLength,
			apiKey:     p.apiKey,
			tracker:    p.tracker,
		},
	}, nil
}

func (p *GeminiProvider) GetEmbedConnection(ctx context.Context, backendID string) (modelrepo.LLMEmbedClient, error) {
	if !p.CanEmbed() {
		return nil, fmt.Errorf("model %s does not support embedding interactions", p.modelName)
	}
	return &GeminiEmbedClient{
		geminiClient: geminiClient{
			modelName:  p.modelName,
			baseURL:    p.baseURL,
			httpClient: p.httpClient,
			apiKey:     p.apiKey,
			tracker:    p.tracker,
		},
	}, nil
}

func (p *GeminiProvider) GetStreamConnection(ctx context.Context, backendID string) (modelrepo.LLMStreamClient, error) {
	if !p.CanStream() {
		return nil, fmt.Errorf("model %s does not support streaming interactions", p.modelName)
	}
	return &GeminiStreamClient{
		geminiClient: geminiClient{
			modelName:  p.modelName,
			baseURL:    p.baseURL,
			httpClient: p.httpClient,
			maxTokens:  p.contextLength,
			apiKey:     p.apiKey,
			tracker:    p.tracker,
		},
	}, nil
}

package vertex

import (
	"context"
	"fmt"
	"net/http"

	"github.com/contenox/contenox/internal/modelrepo"
	"github.com/contenox/contenox/libtracker"
)

type vertexProvider struct {
	id            string
	publisher     string
	modelName     string
	baseURL       string
	credJSON      string // service account JSON; empty → ADC
	httpClient    *http.Client
	contextLength int
	canChat       bool
	canPrompt     bool
	canEmbed      bool
	canStream     bool
	tracker       libtracker.ActivityTracker
}

// NewVertexProvider returns a modelrepo.Provider for a Vertex AI model.
// credJSON is the service account key JSON; empty string falls back to ADC.
func NewVertexProvider(publisher, modelName string, baseURLs []string, cap modelrepo.CapabilityConfig, credJSON string, httpClient *http.Client, tracker libtracker.ActivityTracker) modelrepo.Provider {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	if tracker == nil {
		tracker = libtracker.NoopTracker{}
	}
	baseURL := ""
	if len(baseURLs) > 0 {
		baseURL = baseURLs[0]
	}
	return &vertexProvider{
		id:            fmt.Sprintf("vertex-%s:%s", publisher, modelName),
		publisher:     publisher,
		modelName:     modelName,
		baseURL:       baseURL,
		credJSON:      credJSON,
		httpClient:    httpClient,
		contextLength: cap.ContextLength,
		canChat:       cap.CanChat,
		canPrompt:     cap.CanPrompt,
		canEmbed:      cap.CanEmbed,
		canStream:     cap.CanStream,
		tracker:       tracker,
	}
}

func (p *vertexProvider) GetBackendIDs() []string { return []string{p.baseURL} }
func (p *vertexProvider) ModelName() string       { return p.modelName }
func (p *vertexProvider) GetID() string           { return p.id }
func (p *vertexProvider) GetType() string         { return "vertex-" + p.publisher }
func (p *vertexProvider) GetContextLength() int   { return p.contextLength }
func (p *vertexProvider) CanChat() bool           { return p.canChat }
func (p *vertexProvider) CanEmbed() bool          { return p.canEmbed }
func (p *vertexProvider) CanStream() bool         { return p.canStream }
func (p *vertexProvider) CanPrompt() bool         { return p.canPrompt }
func (p *vertexProvider) CanThink() bool          { return false }

func (p *vertexProvider) client() vertexClient {
	return vertexClient{
		baseURL:       p.baseURL,
		publisher:     p.publisher,
		modelName:     p.modelName,
		contextLength: p.contextLength,
		credJSON:      p.credJSON,
		httpClient:    p.httpClient,
		tracker:       p.tracker,
	}
}

func (p *vertexProvider) GetChatConnection(_ context.Context, _ string) (modelrepo.LLMChatClient, error) {
	if !p.CanChat() {
		return nil, fmt.Errorf("model %s does not support chat interactions", p.modelName)
	}
	c := p.client()
	return &vertexChatClient{vertexClient: c}, nil
}

func (p *vertexProvider) GetPromptConnection(_ context.Context, _ string) (modelrepo.LLMPromptExecClient, error) {
	if !p.CanPrompt() {
		return nil, fmt.Errorf("model %s does not support prompt interactions", p.modelName)
	}
	c := p.client()
	return &vertexPromptClient{vertexClient: c}, nil
}

func (p *vertexProvider) GetStreamConnection(_ context.Context, _ string) (modelrepo.LLMStreamClient, error) {
	if !p.CanStream() {
		return nil, fmt.Errorf("model %s does not support streaming interactions", p.modelName)
	}
	c := p.client()
	return &vertexStreamClient{vertexClient: c}, nil
}

func (p *vertexProvider) GetEmbedConnection(_ context.Context, _ string) (modelrepo.LLMEmbedClient, error) {
	return nil, fmt.Errorf("model %s (vertex-%s) does not support embeddings", p.modelName, p.publisher)
}

var _ modelrepo.Provider = (*vertexProvider)(nil)

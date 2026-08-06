package vertex

import (
	"context"
	"fmt"
	"net/http"
	"sync"

	"golang.org/x/oauth2"

	"github.com/contenox/contenox/internal/models/modelrepo"
	"github.com/contenox/contenox/libtracker"
)

type vertexProvider struct {
	id              string
	publisher       string
	modelName       string
	baseURL         string
	credJSON        string // service account JSON; empty → ADC
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

	// Cached token source. Initialized once on first use and reused across all
	// requests; oauth2 keeps the access token in memory until expiry, so
	// steady-state requests don't hit the token endpoint at all.
	tokenOnce sync.Once
	tokenSrc  oauth2.TokenSource
	tokenErr  error
}

// NewVertexProvider returns a modelrepo.Provider for a Vertex AI model.
// credJSON is the service account key JSON; empty string falls back to ADC.
func NewVertexProvider(publisher, modelName string, baseURLs []string, cap modelrepo.CapabilityConfig, credJSON string, httpClient *http.Client, tracker libtracker.ActivityTracker) modelrepo.Provider {
	if httpClient == nil {
		httpClient = modelrepo.SharedHTTPClient
	}
	if tracker == nil {
		tracker = libtracker.NoopTracker{}
	}
	baseURL := ""
	if len(baseURLs) > 0 {
		baseURL = baseURLs[0]
	}
	maxOut := cap.MaxOutputTokens
	if maxOut <= 0 {
		maxOut = 65536 // Vertex hard ceiling: exclusive upper bound is 65537
	}
	return &vertexProvider{
		id:              fmt.Sprintf("vertex-%s:%s", publisher, modelName),
		publisher:       publisher,
		modelName:       modelName,
		baseURL:         baseURL,
		credJSON:        credJSON,
		httpClient:      httpClient,
		contextLength:   cap.ContextLength,
		maxOutputTokens: maxOut,
		canChat:         cap.CanChat,
		canPrompt:       cap.CanPrompt,
		canEmbed:        cap.CanEmbed,
		canStream:       cap.CanStream,
		canThink:        cap.CanThink,
		canVision:       cap.CanVision,
		tracker:         tracker,
	}
}

func (p *vertexProvider) GetBackendIDs() []string { return []string{p.baseURL} }
func (p *vertexProvider) ModelName() string       { return p.modelName }
func (p *vertexProvider) GetID() string           { return p.id }
func (p *vertexProvider) GetType() string         { return "vertex-" + p.publisher }
func (p *vertexProvider) GetContextLength() int   { return p.contextLength }
func (p *vertexProvider) GetMaxOutputTokens() int { return p.maxOutputTokens }
func (p *vertexProvider) CanChat() bool           { return p.canChat }
func (p *vertexProvider) CanEmbed() bool          { return p.canEmbed }
func (p *vertexProvider) CanStream() bool         { return p.canStream }
func (p *vertexProvider) CanPrompt() bool         { return p.canPrompt }
func (p *vertexProvider) CanThink() bool          { return p.canThink }
func (p *vertexProvider) CanVision() bool         { return p.canVision }

// tokenFn returns an access token using the provider's cached oauth2 source.
// The source is built once on first use (with context.Background so it
// outlives the request that triggered initialization) and reused thereafter.
func (p *vertexProvider) tokenFn(_ context.Context) (string, error) {
	p.tokenOnce.Do(func() {
		p.tokenSrc, p.tokenErr = NewTokenSource(context.Background(), p.credJSON)
	})
	if p.tokenErr != nil {
		return "", p.tokenErr
	}
	tok, err := p.tokenSrc.Token()
	if err != nil {
		return "", fmt.Errorf("vertex AI token: %w", err)
	}
	return tok.AccessToken, nil
}

func (p *vertexProvider) client() vertexClient {
	return vertexClient{
		baseURL:         p.baseURL,
		publisher:       p.publisher,
		modelName:       p.modelName,
		contextLength:   p.contextLength,
		maxOutputTokens: p.maxOutputTokens,
		credJSON:        p.credJSON,
		httpClient:      p.httpClient,
		canThink:        p.canThink,
		tracker:         p.tracker,
		tokenFn:         p.tokenFn,
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
	if !p.CanEmbed() {
		return nil, fmt.Errorf("model %s (vertex-%s) does not support embeddings; use a text-embedding model such as gemini-embedding-001", p.modelName, p.publisher)
	}
	c := p.client()
	return &vertexEmbedClient{vertexClient: c}, nil
}

var _ modelrepo.Provider = (*vertexProvider)(nil)

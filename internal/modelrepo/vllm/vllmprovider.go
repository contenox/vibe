package vllm

import (
	"context"
	"fmt"
	"net/http"
	"net/url"

	"github.com/contenox/vibe/internal/modelrepo"
	"github.com/contenox/vibe/libtracker"
)

type vLLMProvider struct {
	Name           string
	ID             string
	ContextLength  int
	SupportsChat   bool
	SupportsEmbed  bool
	SupportsStream bool
	SupportsPrompt bool
	Backends       []string
	authToken      string
	client         *http.Client
	tracker        libtracker.ActivityTracker
}

func NewVLLMProvider(modelName string, backends []string, client *http.Client, caps modelrepo.CapabilityConfig, authToken string, tracker libtracker.ActivityTracker) modelrepo.Provider {
	if tracker == nil {
		tracker = libtracker.NoopTracker{}
	}
	return &vLLMProvider{
		Name:           modelName,
		ID:             "vllm:" + modelName,
		ContextLength:  caps.ContextLength,
		SupportsChat:   caps.CanChat,
		SupportsEmbed:  caps.CanEmbed,
		SupportsStream: caps.CanStream,
		SupportsPrompt: caps.CanPrompt,
		Backends:       backends,
		authToken:      authToken,
		client:         client,
		tracker:        tracker,
	}
}

func (p *vLLMProvider) validateBackend(backendID string) error {
	u, err := url.Parse(backendID)
	if err != nil {
		return fmt.Errorf("invalid backend URL: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("invalid URL scheme (must be http/https): %s", backendID)
	}
	if u.Host == "" {
		return fmt.Errorf("missing host in backend URL: %s", backendID)
	}
	return nil
}

func (p *vLLMProvider) GetBackendIDs() []string {
	return p.Backends
}

func (p *vLLMProvider) ModelName() string {
	return p.Name
}

func (p *vLLMProvider) GetID() string {
	return p.ID
}

func (p *vLLMProvider) GetType() string {
	return "vllm"
}

func (p *vLLMProvider) GetContextLength() int {
	return p.ContextLength
}

func (p *vLLMProvider) CanChat() bool {
	return p.SupportsChat
}

func (p *vLLMProvider) CanEmbed() bool {
	return p.SupportsEmbed
}

func (p *vLLMProvider) CanStream() bool {
	return p.SupportsStream
}

func (p *vLLMProvider) CanPrompt() bool {
	return p.SupportsPrompt
}

func (p *vLLMProvider) CanThink() bool {
	return false
}

func (p *vLLMProvider) GetChatConnection(ctx context.Context, backendID string) (modelrepo.LLMChatClient, error) {
	if !p.CanChat() {
		return nil, fmt.Errorf("provider %s (model %s) does not support chat", p.GetID(), p.ModelName())
	}
	if err := p.validateBackend(backendID); err != nil {
		return nil, err
	}

	return NewVLLMChatClient(ctx, backendID, p.ModelName(), p.ContextLength, p.client, p.authToken, p.tracker)
}

func (p *vLLMProvider) GetPromptConnection(ctx context.Context, backendID string) (modelrepo.LLMPromptExecClient, error) {
	if !p.CanPrompt() {
		return nil, fmt.Errorf("provider %s (model %s) does not support prompting", p.GetID(), p.ModelName())
	}

	if err := p.validateBackend(backendID); err != nil {
		return nil, err
	}

	return NewVLLMPromptClient(ctx, backendID, p.ModelName(), p.ContextLength, p.client, p.authToken, p.tracker)
}

func (p *vLLMProvider) GetEmbedConnection(ctx context.Context, backendID string) (modelrepo.LLMEmbedClient, error) {
	return nil, fmt.Errorf("provider %s (model %s) does not support embeddings", p.GetID(), p.ModelName())
}

func (p *vLLMProvider) GetStreamConnection(ctx context.Context, backendID string) (modelrepo.LLMStreamClient, error) {
	if !p.CanStream() {
		return nil, fmt.Errorf("provider %s (model %s) does not support streaming", p.GetID(), p.ModelName())
	}

	if err := p.validateBackend(backendID); err != nil {
		return nil, err
	}

	return NewVLLMStreamClient(ctx, backendID, p.ModelName(), p.ContextLength, p.client, p.authToken, p.tracker)
}

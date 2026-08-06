package bedrock

import (
	"context"
	"fmt"
	"net/http"
	"sync"

	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"

	"github.com/contenox/contenox/internal/models/modelrepo"
	"github.com/contenox/contenox/libtracker"
)

type bedrockProvider struct {
	id              string
	region          string
	credBlob        string // optional stored static-credentials JSON; empty → ambient chain
	modelName       string
	httpClient      *http.Client
	contextLength   int
	maxOutputTokens int
	canChat         bool
	canPrompt       bool
	canStream       bool
	canThink        bool
	canVision       bool
	tracker         libtracker.ActivityTracker

	// aws.Config / SDK client built once and reused (mirrors vertex tokenOnce).
	once   sync.Once
	api    *bedrockruntime.Client
	apiErr error
}

// NewBedrockProvider returns a modelrepo.Provider for an AWS Bedrock model via
// the Converse API. credBlob is optional static-credentials JSON; empty falls
// back to the ambient AWS credential chain.
func NewBedrockProvider(region, credBlob, modelName string, cap modelrepo.CapabilityConfig, httpClient *http.Client, tracker libtracker.ActivityTracker) modelrepo.Provider {
	if tracker == nil {
		tracker = libtracker.NoopTracker{}
	}
	return &bedrockProvider{
		id:              fmt.Sprintf("bedrock-%s", modelName),
		region:          region,
		credBlob:        credBlob,
		modelName:       modelName,
		httpClient:      httpClient,
		contextLength:   cap.ContextLength,
		maxOutputTokens: cap.MaxOutputTokens,
		canChat:         cap.CanChat,
		canPrompt:       cap.CanPrompt,
		canStream:       cap.CanStream,
		canThink:        cap.CanThink,
		canVision:       cap.CanVision,
		tracker:         tracker,
	}
}

func (p *bedrockProvider) GetBackendIDs() []string { return []string{"bedrock:" + p.region} }
func (p *bedrockProvider) ModelName() string       { return p.modelName }
func (p *bedrockProvider) GetID() string           { return p.id }
func (p *bedrockProvider) GetType() string         { return "bedrock" }
func (p *bedrockProvider) GetContextLength() int   { return p.contextLength }
func (p *bedrockProvider) GetMaxOutputTokens() int { return p.maxOutputTokens }
func (p *bedrockProvider) CanChat() bool           { return p.canChat }
func (p *bedrockProvider) CanEmbed() bool          { return false }
func (p *bedrockProvider) CanStream() bool         { return p.canStream }
func (p *bedrockProvider) CanPrompt() bool         { return p.canPrompt }
func (p *bedrockProvider) CanThink() bool          { return p.canThink }
func (p *bedrockProvider) CanVision() bool         { return p.canVision }

func (p *bedrockProvider) client(ctx context.Context) (bedrockClient, error) {
	p.once.Do(func() {
		cfg, err := loadAWSConfig(ctx, p.region, p.credBlob, p.httpClient)
		if err != nil {
			p.apiErr = err
			return
		}
		p.api = bedrockruntime.NewFromConfig(cfg)
	})
	if p.apiErr != nil {
		return bedrockClient{}, p.apiErr
	}
	return bedrockClient{api: p.api, modelName: p.modelName, maxOutputTokens: p.maxOutputTokens, tracker: p.tracker}, nil
}

func (p *bedrockProvider) GetChatConnection(ctx context.Context, _ string) (modelrepo.LLMChatClient, error) {
	if !p.CanChat() {
		return nil, fmt.Errorf("model %s does not support chat interactions", p.modelName)
	}
	c, err := p.client(ctx)
	if err != nil {
		return nil, err
	}
	return &bedrockChatClient{c}, nil
}

func (p *bedrockProvider) GetStreamConnection(ctx context.Context, _ string) (modelrepo.LLMStreamClient, error) {
	if !p.CanStream() {
		return nil, fmt.Errorf("model %s does not support streaming interactions", p.modelName)
	}
	c, err := p.client(ctx)
	if err != nil {
		return nil, err
	}
	return &bedrockStreamClient{c}, nil
}

func (p *bedrockProvider) GetPromptConnection(ctx context.Context, _ string) (modelrepo.LLMPromptExecClient, error) {
	if !p.CanPrompt() {
		return nil, fmt.Errorf("model %s does not support prompt interactions", p.modelName)
	}
	c, err := p.client(ctx)
	if err != nil {
		return nil, err
	}
	return &bedrockPromptClient{c}, nil
}

func (p *bedrockProvider) GetEmbedConnection(_ context.Context, _ string) (modelrepo.LLMEmbedClient, error) {
	return nil, fmt.Errorf("model %s (bedrock) does not support embeddings via Converse", p.modelName)
}

var _ modelrepo.Provider = (*bedrockProvider)(nil)

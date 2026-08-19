package modelrepo

import (
	"context"
	"errors"
	"fmt"
)

// MockProvider carries capability flags for resolver tests only; its clients refuse to answer, because a fabricated reply is what let a broken chain look green.
type MockProvider struct {
	ID              string
	Name            string
	ContextLength   int
	MaxOutputTokens int
	CanChatFlag     bool
	CanEmbedFlag    bool
	CanStreamFlag   bool
	CanPromptFlag   bool
	CanVisionFlag   bool
	CanAudioFlag    bool
	Backends        []string
}

func (m *MockProvider) GetBackendIDs() []string {
	return m.Backends
}

func (m *MockProvider) ModelName() string {
	return m.Name
}

func (m *MockProvider) GetID() string {
	return m.ID
}

func (m *MockProvider) GetType() string {
	return "mock"
}

func (m *MockProvider) noDialog(operation string) error {
	name := "mock"
	if m != nil && m.Name != "" {
		name = m.Name
	}
	return fmt.Errorf("%w: mock provider %q was asked to %s; register a %s backend for a replayable dialog", ErrNoDialog, name, operation, ScriptedTestBackendType)
}

func (m *MockProvider) GetContextLength() int { return m.ContextLength }

func (m *MockProvider) GetMaxOutputTokens() int { return m.MaxOutputTokens }

func (m *MockProvider) CanChat() bool {
	return m.CanChatFlag
}

func (m *MockProvider) CanEmbed() bool {
	return m.CanEmbedFlag
}

func (m *MockProvider) CanStream() bool {
	return m.CanStreamFlag
}

func (m *MockProvider) CanPrompt() bool {
	return m.CanPromptFlag
}

func (m *MockProvider) CanThink() bool {
	return false
}

func (m *MockProvider) CanVision() bool {
	return m.CanVisionFlag
}

func (m *MockProvider) CanAudio() bool {
	return m.CanAudioFlag
}

func (m *MockProvider) GetChatConnection(ctx context.Context, backendID string) (LLMChatClient, error) {
	if !m.CanChat() {
		return nil, ErrNotSupported
	}
	return &MockChatClient{provider: m}, nil
}

func (m *MockProvider) GetPromptConnection(ctx context.Context, backendID string) (LLMPromptExecClient, error) {
	if !m.CanPrompt() {
		return nil, ErrNotSupported
	}
	return &MockPromptClient{provider: m}, nil
}

func (m *MockProvider) GetEmbedConnection(ctx context.Context, backendID string) (LLMEmbedClient, error) {
	if !m.CanEmbed() {
		return nil, ErrNotSupported
	}
	return &MockEmbedClient{provider: m}, nil
}

func (m *MockProvider) GetStreamConnection(ctx context.Context, backendID string) (LLMStreamClient, error) {
	if !m.CanStream() {
		return nil, ErrNotSupported
	}
	return &MockStreamClient{provider: m}, nil
}

type MockChatClient struct{ provider *MockProvider }

func (m *MockChatClient) Chat(ctx context.Context, messages []Message, opts ...ChatArgument) (ChatResult, error) {
	return ChatResult{}, m.provider.noDialog("chat")
}

func (m *MockChatClient) Close() error {
	return nil
}

type MockPromptClient struct{ provider *MockProvider }

func (m *MockPromptClient) Prompt(ctx context.Context, systemInstruction string, temperature float32, prompt string) (string, *TokenUsage, error) {
	return "", nil, m.provider.noDialog("prompt")
}

func (m *MockPromptClient) Close() error {
	return nil
}

type MockEmbedClient struct{ provider *MockProvider }

func (m *MockEmbedClient) Embed(ctx context.Context, prompt string) ([]float64, error) {
	return nil, m.provider.noDialog("embed")
}

func (m *MockEmbedClient) Close() error {
	return nil
}

type MockStreamClient struct{ provider *MockProvider }

func (m *MockStreamClient) Stream(ctx context.Context, messages []Message, args ...ChatArgument) (<-chan *StreamParcel, error) {
	return nil, m.provider.noDialog("stream")
}

func (m *MockStreamClient) Close() error {
	return nil
}

// ErrNotSupported is returned by a connection getter when the matching Can* flag is false.
var ErrNotSupported = errors.New("operation not supported")

// ErrNoDialog is returned by every MockProvider client: capability flags are all it has, and a model reply is not something it may invent.
var ErrNoDialog = errors.New("no dialog behind this provider")

var _ Provider = &MockProvider{}

package modelrepo

import (
	"context"
	"errors"
)

// MockProvider is a mock implementation of the Provider interface for testing.
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

func (m *MockProvider) GetChatConnection(ctx context.Context, backendID string) (LLMChatClient, error) {
	if !m.CanChat() {
		return nil, ErrNotSupported
	}
	return &MockChatClient{}, nil
}

func (m *MockProvider) GetPromptConnection(ctx context.Context, backendID string) (LLMPromptExecClient, error) {
	if !m.CanPrompt() {
		return nil, ErrNotSupported
	}
	return &MockPromptClient{}, nil
}

func (m *MockProvider) GetEmbedConnection(ctx context.Context, backendID string) (LLMEmbedClient, error) {
	if !m.CanEmbed() {
		return nil, ErrNotSupported
	}
	return &MockEmbedClient{}, nil
}

func (m *MockProvider) GetStreamConnection(ctx context.Context, backendID string) (LLMStreamClient, error) {
	if !m.CanStream() {
		return nil, ErrNotSupported
	}
	return &MockStreamClient{}, nil
}

// MockChatClient is a mock implementation of LLMChatClient for testing.
type MockChatClient struct{}

func (m *MockChatClient) Chat(ctx context.Context, messages []Message, opts ...ChatArgument) (ChatResult, error) {
	return ChatResult{
		Message: Message{Role: "assistant", Content: "mock response"},
	}, nil
}

func (m *MockChatClient) Close() error {
	return nil
}

// MockPromptClient is a mock implementation of LLMPromptExecClient for testing.
type MockPromptClient struct{}

func (m *MockPromptClient) Prompt(ctx context.Context, systemInstruction string, temperature float32, prompt string) (string, *TokenUsage, error) {
	return "mock response", nil, nil
}

func (m *MockPromptClient) Close() error {
	return nil
}

// MockEmbedClient is a mock implementation of LLMEmbedClient for testing.
type MockEmbedClient struct{}

func (m *MockEmbedClient) Embed(ctx context.Context, prompt string) ([]float64, error) {
	return []float64{0.1, 0.2, 0.3}, nil
}

func (m *MockEmbedClient) Close() error {
	return nil
}

// MockStreamClient is a mock implementation of LLMStreamClient for testing.
type MockStreamClient struct{}

// Stream honors the raw-delta contract's terminal parcel.
func (m *MockStreamClient) Stream(ctx context.Context, messages []Message, args ...ChatArgument) (<-chan *StreamParcel, error) {
	ch := make(chan *StreamParcel)
	go func() {
		defer close(ch)
		ch <- &StreamParcel{Data: "mock data part 1"}
		ch <- &StreamParcel{Data: "mock data part 2"}
		ch <- &StreamParcel{Data: "mock data part 3"}
		ch <- &StreamParcel{Terminal: &StreamTerminal{FinishReason: "stop"}}
	}()
	return ch, nil
}

func (m *MockStreamClient) Close() error {
	return nil
}

// ErrNotSupported is returned by a connection getter when the matching Can* flag is false.
var ErrNotSupported = errors.New("operation not supported")

var _ Provider = &MockProvider{}

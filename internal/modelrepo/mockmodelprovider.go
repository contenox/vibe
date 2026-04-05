package modelrepo

import (
	"context"
	"errors"
)

// MockProvider is a mock implementation of the Provider interface for testing.
type MockProvider struct {
	ID            string
	Name          string
	ContextLength int
	CanChatFlag   bool
	CanEmbedFlag  bool
	CanStreamFlag bool
	CanPromptFlag bool
	Backends      []string
}

// GetBackendIDs returns the backend IDs for the mock provider.
func (m *MockProvider) GetBackendIDs() []string {
	return m.Backends
}

// ModelName returns the model name for the mock provider.
func (m *MockProvider) ModelName() string {
	return m.Name
}

// GetID returns the ID for the mock provider.
func (m *MockProvider) GetID() string {
	return m.ID
}

// GetType returns the provider type for the mock provider.
func (m *MockProvider) GetType() string {
	return "mock"
}

// GetContextLength returns the context length for the mock provider.
func (m *MockProvider) GetContextLength() int {
	return m.ContextLength
}

// CanChat returns whether the mock provider can chat.
func (m *MockProvider) CanChat() bool {
	return m.CanChatFlag
}

// CanEmbed returns whether the mock provider can embed.
func (m *MockProvider) CanEmbed() bool {
	return m.CanEmbedFlag
}

// CanStream returns whether the mock provider can stream.
func (m *MockProvider) CanStream() bool {
	return m.CanStreamFlag
}

// CanPrompt returns whether the mock provider can prompt.
func (m *MockProvider) CanPrompt() bool {
	return m.CanPromptFlag
}

// CanThink returns whether the mock provider can think.
func (m *MockProvider) CanThink() bool {
	return false
}

// GetChatConnection returns a mock chat client.
func (m *MockProvider) GetChatConnection(ctx context.Context, backendID string) (LLMChatClient, error) {
	if !m.CanChat() {
		return nil, ErrNotSupported
	}
	return &MockChatClient{}, nil
}

// GetPromptConnection returns a mock prompt client.
func (m *MockProvider) GetPromptConnection(ctx context.Context, backendID string) (LLMPromptExecClient, error) {
	if !m.CanPrompt() {
		return nil, ErrNotSupported
	}
	return &MockPromptClient{}, nil
}

// GetEmbedConnection returns a mock embed client.
func (m *MockProvider) GetEmbedConnection(ctx context.Context, backendID string) (LLMEmbedClient, error) {
	if !m.CanEmbed() {
		return nil, ErrNotSupported
	}
	return &MockEmbedClient{}, nil
}

// GetStreamConnection returns a mock stream client.
func (m *MockProvider) GetStreamConnection(ctx context.Context, backendID string) (LLMStreamClient, error) {
	if !m.CanStream() {
		return nil, ErrNotSupported
	}
	return &MockStreamClient{}, nil
}

// MockChatClient is a mock implementation of LLMChatClient for testing.
type MockChatClient struct{}

// Chat returns a mock response.
func (m *MockChatClient) Chat(ctx context.Context, messages []Message, opts ...ChatArgument) (ChatResult, error) {
	return ChatResult{
		Message: Message{Role: "assistant", Content: "mock response"},
	}, nil
}

// Close is a no-op for the mock client.
func (m *MockChatClient) Close() error {
	return nil
}

// MockPromptClient is a mock implementation of LLMPromptExecClient for testing.
type MockPromptClient struct{}

// Prompt returns a mock response.
func (m *MockPromptClient) Prompt(ctx context.Context, systemInstruction string, temperature float32, prompt string) (string, error) {
	return "mock response", nil
}

// Close is a no-op for the mock client.
func (m *MockPromptClient) Close() error {
	return nil
}

// MockEmbedClient is a mock implementation of LLMEmbedClient for testing.
type MockEmbedClient struct{}

// Embed returns a mock embedding.
func (m *MockEmbedClient) Embed(ctx context.Context, prompt string) ([]float64, error) {
	return []float64{0.1, 0.2, 0.3}, nil
}

// Close is a no-op for the mock client.
func (m *MockEmbedClient) Close() error {
	return nil
}

// MockStreamClient is a mock implementation of LLMStreamClient for testing.
type MockStreamClient struct{}

// Stream returns a channel with mock stream parcels.
func (m *MockStreamClient) Stream(ctx context.Context, messages []Message, args ...ChatArgument) (<-chan *StreamParcel, error) {
	ch := make(chan *StreamParcel)
	go func() {
		defer close(ch)
		ch <- &StreamParcel{Data: "mock data part 1"}
		ch <- &StreamParcel{Data: "mock data part 2"}
		ch <- &StreamParcel{Data: "mock data part 3"}
	}()
	return ch, nil
}

// Close is a no-op for the mock client.
func (m *MockStreamClient) Close() error {
	return nil
}

// ErrNotSupported is returned when an operation is not supported.
var ErrNotSupported = errors.New("operation not supported")

var _ Provider = &MockProvider{}

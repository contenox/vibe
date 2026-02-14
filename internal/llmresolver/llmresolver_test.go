package llmresolver_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/contenox/vibe/internal/llmresolver"
	libmodelprovider "github.com/contenox/vibe/internal/modelrepo"
)

func TestUnit_ChatModelResolution(t *testing.T) {
	tests := []struct {
		name        string
		req         llmresolver.Request
		providers   []libmodelprovider.Provider
		wantErr     error
		wantModelID string
	}{
		{
			name: "happy path - exact model match",
			req: llmresolver.Request{
				ModelNames:    []string{"llama2:latest"},
				ContextLength: 4096,
			},
			providers: []libmodelprovider.Provider{
				&libmodelprovider.MockProvider{
					ID:            "1",
					Name:          "llama2:latest",
					ContextLength: 4096,
					CanChatFlag:   true,
					Backends:      []string{"b1"},
				},
			},
			wantModelID: "1",
		},
		{
			name: "no models available",
			req: llmresolver.Request{
				ContextLength: 1,
			},
			providers: []libmodelprovider.Provider{},
			wantErr:   llmresolver.ErrNoAvailableModels,
		},
		{
			name: "insufficient context length",
			req: llmresolver.Request{
				ContextLength: 8000,
			},
			providers: []libmodelprovider.Provider{
				&libmodelprovider.MockProvider{
					ContextLength: 4096,
					CanChatFlag:   true,
				},
			},
			wantErr: llmresolver.ErrNoSatisfactoryModel,
		},
		{
			name: "model exists but name mismatch",
			req: llmresolver.Request{
				ModelNames:    []string{"smollm2:135m"},
				ContextLength: 1,
			},
			providers: []libmodelprovider.Provider{
				&libmodelprovider.MockProvider{
					ID:            "2",
					Name:          "smollm2",
					ContextLength: 4096,
					CanChatFlag:   true,
					Backends:      []string{"b2"},
				},
			},
		},
		{
			name: "partial match after normalization - tag stripped",
			req: llmresolver.Request{
				ModelNames:    []string{"llama2:7b"},
				ContextLength: 4096,
			},
			providers: []libmodelprovider.Provider{
				&libmodelprovider.MockProvider{
					ID:            "3",
					Name:          "llama2",
					ContextLength: 4096,
					CanChatFlag:   true,
					Backends:      []string{"b3"},
				},
			},
			wantModelID: "3",
		},
		{
			name: "case-insensitive match after normalization",
			req: llmresolver.Request{
				ModelNames:    []string{"Llama2"},
				ContextLength: 4096,
			},
			providers: []libmodelprovider.Provider{
				&libmodelprovider.MockProvider{
					ID:            "4",
					Name:          "llama2",
					ContextLength: 4096,
					CanChatFlag:   true,
					Backends:      []string{"b4"},
				},
			},
			wantModelID: "4",
		},
		{
			name: "quantization suffix stripped - awq",
			req: llmresolver.Request{
				ModelNames:    []string{"llama2-awq"},
				ContextLength: 4096,
			},
			providers: []libmodelprovider.Provider{
				&libmodelprovider.MockProvider{
					ID:            "5",
					Name:          "llama2",
					ContextLength: 4096,
					CanChatFlag:   true,
					Backends:      []string{"b5"},
				},
			},
			wantModelID: "5",
		},
		{
			name: "multiple model names, first not found, second found",
			req: llmresolver.Request{
				ModelNames:    []string{"nonexistent", "llama2"},
				ContextLength: 4096,
			},
			providers: []libmodelprovider.Provider{
				&libmodelprovider.MockProvider{
					ID:            "6",
					Name:          "llama2",
					ContextLength: 4096,
					CanChatFlag:   true,
					Backends:      []string{"b6"},
				},
			},
			wantModelID: "6",
		},
		{
			name: "empty model names (any allowed)",
			req: llmresolver.Request{
				ContextLength: 4096,
			},
			providers: []libmodelprovider.Provider{
				&libmodelprovider.MockProvider{
					ID:            "7",
					Name:          "llama2",
					ContextLength: 4096,
					CanChatFlag:   true,
					Backends:      []string{"b7"},
				},
			},
			wantModelID: "7",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			getModels := func(_ context.Context, _ ...string) ([]libmodelprovider.Provider, error) {
				return tt.providers, nil
			}

			_, provider, _, err := llmresolver.Chat(context.Background(), tt.req, getModels, llmresolver.Randomly)

			// Check error condition
			if !errors.Is(err, tt.wantErr) {
				t.Errorf("got error %v, want %v", err, tt.wantErr)
			}

			// Check provider ID if expected
			if tt.wantModelID != "" {
				if provider == nil {
					t.Errorf("expected provider with ID %s, got nil", tt.wantModelID)
				} else if provider.GetID() != tt.wantModelID {
					t.Errorf("got provider ID %s, want %s", provider.GetID(), tt.wantModelID)
				}
			}
		})
	}
}

func TestUnit_EmbedModelResolution(t *testing.T) {
	// Define common providers used in tests
	providerEmbedOK := &libmodelprovider.MockProvider{
		ID:           "p1",
		Name:         "text-embed-model",
		CanEmbedFlag: true,
		Backends:     []string{"b1"},
	}
	providerEmbedNoBackends := &libmodelprovider.MockProvider{
		ID:           "p2",
		Name:         "text-embed-model",
		CanEmbedFlag: true,
		Backends:     []string{}, // No backends
	}
	providerEmbedCannotEmbed := &libmodelprovider.MockProvider{
		ID:           "p4",
		Name:         "text-embed-model",
		CanEmbedFlag: false, // Cannot embed
		Backends:     []string{"b4"},
	}

	tests := []struct {
		name      string
		embedReq  llmresolver.EmbedRequest
		providers []libmodelprovider.Provider
		resolver  func(candidates []libmodelprovider.Provider) (libmodelprovider.Provider, string, error)
		wantErr   error
		wantMsg   string
	}{
		{
			name:      "happy path - exact model match",
			embedReq:  llmresolver.EmbedRequest{ModelName: "text-embed-model"},
			providers: []libmodelprovider.Provider{providerEmbedOK},
			resolver:  llmresolver.Randomly,
			wantErr:   nil,
		},
		{
			name:      "error - model name required",
			embedReq:  llmresolver.EmbedRequest{ModelName: ""},
			providers: []libmodelprovider.Provider{providerEmbedOK},
			resolver:  llmresolver.Randomly,
			wantErr:   fmt.Errorf("model name is required"),
			wantMsg:   "model name is required",
		},
		{
			name:      "error - no models available",
			embedReq:  llmresolver.EmbedRequest{ModelName: "text-embed-model"},
			providers: []libmodelprovider.Provider{},
			resolver:  llmresolver.Randomly,
			wantErr:   llmresolver.ErrNoAvailableModels,
		},
		{
			name:      "error - no satisfactory model (name mismatch)",
			embedReq:  llmresolver.EmbedRequest{ModelName: "non-existent-model"},
			providers: []libmodelprovider.Provider{providerEmbedOK},
			resolver:  llmresolver.Randomly,
			wantErr:   llmresolver.ErrNoSatisfactoryModel,
		},
		{
			name:      "error - no satisfactory model (capability mismatch)",
			embedReq:  llmresolver.EmbedRequest{ModelName: "text-embed-model"},
			providers: []libmodelprovider.Provider{providerEmbedCannotEmbed},
			resolver:  llmresolver.Randomly,
			wantErr:   llmresolver.ErrNoSatisfactoryModel,
		},
		{
			name:      "error - selected provider has no backends",
			embedReq:  llmresolver.EmbedRequest{ModelName: "text-embed-model"},
			providers: []libmodelprovider.Provider{providerEmbedNoBackends},
			resolver:  llmresolver.Randomly,
			// Error comes from selectRandomBackend called by ResolveRandomly
			wantErr: llmresolver.ErrNoSatisfactoryModel,
		},
		{
			name:      "multiple candidates - resolver selects one",
			embedReq:  llmresolver.EmbedRequest{ModelName: "text-embed-model"},
			providers: []libmodelprovider.Provider{providerEmbedOK, &libmodelprovider.MockProvider{ID: "p6", Name: "text-embed-model", CanEmbedFlag: true, Backends: []string{"b6"}}},
			resolver:  llmresolver.Randomly,
			wantErr:   nil,
		},
		{
			name:     "model name with tag matches base",
			embedReq: llmresolver.EmbedRequest{ModelName: "text-embed-model:33m"},
			providers: []libmodelprovider.Provider{
				&libmodelprovider.MockProvider{
					ID:           "p3",
					Name:         "text-embed-model",
					CanEmbedFlag: true,
					Backends:     []string{"b3"},
				},
			},
			resolver: llmresolver.Randomly,
			wantErr:  nil,
		},
		{
			name:     "exact model match with tag",
			embedReq: llmresolver.EmbedRequest{ModelName: "text-embed-model:33m"},
			providers: []libmodelprovider.Provider{
				&libmodelprovider.MockProvider{
					ID:           "p4",
					Name:         "text-embed-model:33m",
					CanEmbedFlag: true,
					Backends:     []string{"b4"},
				},
			},
			resolver: llmresolver.Randomly,
			wantErr:  nil,
		},
		{
			name:     "case-insensitive match after normalization",
			embedReq: llmresolver.EmbedRequest{ModelName: "Text-Embed-Model"},
			providers: []libmodelprovider.Provider{
				providerEmbedOK, // Name: "text-embed-model"
			},
			resolver: llmresolver.Randomly,
			wantErr:  nil,
		},
		{
			name:     "quantization suffix stripped - awq",
			embedReq: llmresolver.EmbedRequest{ModelName: "text-embed-model-awq"},
			providers: []libmodelprovider.Provider{
				&libmodelprovider.MockProvider{
					ID:           "p5",
					Name:         "text-embed-model",
					CanEmbedFlag: true,
					Backends:     []string{"b5"},
				},
			},
			resolver: llmresolver.Randomly,
			wantErr:  nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			getModels := func(_ context.Context, providerTypes ...string) ([]libmodelprovider.Provider, error) {
				return tt.providers, nil
			}

			client, _, _, err := llmresolver.Embed(context.Background(), tt.embedReq, getModels, tt.resolver)

			// Assertions
			if tt.wantErr != nil {
				if tt.wantMsg != "" {
					if err == nil {
						t.Errorf("ResolveEmbed() error = nil, want %q", tt.wantMsg)
					} else if err.Error() != tt.wantMsg {
						t.Errorf("ResolveEmbed() error = %q, want %q", err.Error(), tt.wantMsg)
					}
				} else {
					if !errors.Is(err, tt.wantErr) {
						t.Errorf("ResolveEmbed() error = %v, want %v", err, tt.wantErr)
					}
				}
				if client != nil {
					t.Errorf("ResolveEmbed() client = %v, want nil when error expected", client)
				}
			} else {
				// No error expected
				if err != nil {
					t.Errorf("ResolveEmbed() unexpected error = %v", err)
				}
				if client == nil {
					t.Errorf("ResolveEmbed() client is nil, want non-nil client")
				}
			}
		})
	}
}

func TestUnitNormalizeModelName(t *testing.T) {
	cases := []struct {
		input    string
		expected string
	}{
		{"Llama2:latest", "llama2"},
		{"org/llama2-70b", "llama270b"},
		{"llama2-70b-AWQ", "llama270b"},
		{"llama2_70b-fp16", "llama270b"},
		{"text-embed-model:33m", "textembedmodel"},
		{"smollm2:135m", "smollm2"},
		{"gpt-j-6b", "gptj6b"},
		{"mistral-7b-instruct-v0.1", "mistral7binstructv01"},
	}

	for _, c := range cases {
		t.Run(c.input, func(t *testing.T) {
			got := llmresolver.NormalizeModelName(c.input)
			if got != c.expected {
				t.Errorf("NormalizeModelName(%q) = %q, want %q", c.input, got, c.expected)
			}
		})
	}
}

// Additional tests for the new return values
func TestUnit_ChatReturnsProviderAndBackend(t *testing.T) {
	mockProvider := &libmodelprovider.MockProvider{
		ID:            "test-provider",
		Name:          "test-model",
		ContextLength: 4096,
		CanChatFlag:   true,
		Backends:      []string{"backend-1", "backend-2"},
	}

	getModels := func(_ context.Context, _ ...string) ([]libmodelprovider.Provider, error) {
		return []libmodelprovider.Provider{mockProvider}, nil
	}

	req := llmresolver.Request{
		ModelNames:    []string{"test-model"},
		ContextLength: 4096,
	}

	client, provider, backend, err := llmresolver.Chat(context.Background(), req, getModels, llmresolver.Randomly)

	if err != nil {
		t.Errorf("Unexpected error: %v", err)
	}

	if provider == nil {
		t.Error("Expected non-nil provider")
	} else if provider.GetID() != "test-provider" {
		t.Errorf("Expected provider ID 'test-provider', got '%s'", provider.GetID())
	}

	if backend == "" {
		t.Error("Expected non-empty backend")
	}

	if client == nil {
		t.Error("Expected non-nil client")
	}
}

func TestUnit_EmbedReturnsProviderAndBackend(t *testing.T) {
	mockProvider := &libmodelprovider.MockProvider{
		ID:           "test-embed-provider",
		Name:         "test-embed-model",
		CanEmbedFlag: true,
		Backends:     []string{"embed-backend-1"},
	}

	getModels := func(_ context.Context, _ ...string) ([]libmodelprovider.Provider, error) {
		return []libmodelprovider.Provider{mockProvider}, nil
	}

	embedReq := llmresolver.EmbedRequest{
		ModelName: "test-embed-model",
	}

	client, provider, backend, err := llmresolver.Embed(context.Background(), embedReq, getModels, llmresolver.Randomly)

	if err != nil {
		t.Errorf("Unexpected error: %v", err)
	}

	if provider == nil {
		t.Error("Expected non-nil provider")
	} else if provider.GetID() != "test-embed-provider" {
		t.Errorf("Expected provider ID 'test-embed-provider', got '%s'", provider.GetID())
	}

	if backend != "embed-backend-1" {
		t.Errorf("Expected backend 'embed-backend-1', got '%s'", backend)
	}

	if client == nil {
		t.Error("Expected non-nil client")
	}
}

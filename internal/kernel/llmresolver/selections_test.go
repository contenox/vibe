package llmresolver_test

import (
	"context"
	"testing"

	"github.com/contenox/contenox/internal/kernel/llmresolver"
	libmodelprovider "github.com/contenox/contenox/internal/models/modelrepo"
	"github.com/stretchr/testify/require"
)

// Two backends serving the same model must both survive candidate filtering
// so resolution can fail over between them.
func TestUnit_ChatSelections_KeepsSameModelOnDistinctBackends(t *testing.T) {
	providers := []libmodelprovider.Provider{
		&libmodelprovider.MockProvider{
			ID:          "vertex-google:gemini-test",
			Name:        "gemini-test",
			CanChatFlag: true,
			Backends:    []string{"https://us-central1.example/v1"},
		},
		&libmodelprovider.MockProvider{
			ID:          "vertex-google:gemini-test",
			Name:        "gemini-test",
			CanChatFlag: true,
			Backends:    []string{"https://global.example/v1"},
		},
	}
	getModels := func(context.Context, ...string) ([]libmodelprovider.Provider, error) {
		return providers, nil
	}

	selections, err := llmresolver.ChatSelections(
		context.Background(),
		llmresolver.Request{ModelNames: []string{"gemini-test"}},
		getModels,
		llmresolver.Randomly,
	)
	require.NoError(t, err)
	require.Len(t, selections, 2, "each backend must stay an independent failover candidate")

	backends := map[string]bool{}
	for _, sel := range selections {
		backends[sel.Backend] = true
	}
	require.True(t, backends["https://us-central1.example/v1"])
	require.True(t, backends["https://global.example/v1"])
}

// The ordered selections must start with the policy's pick and cover every
// remaining (provider, backend) pair exactly once.
func TestUnit_StreamSelections_PolicyPickFirstThenAlternates(t *testing.T) {
	pinned := &libmodelprovider.MockProvider{
		ID:            "vertex-google:gemini-test",
		Name:          "gemini-test",
		CanStreamFlag: true,
		Backends:      []string{"https://global.example/v1"},
	}
	other := &libmodelprovider.MockProvider{
		ID:            "vertex-google:gemini-test",
		Name:          "gemini-test",
		CanStreamFlag: true,
		Backends:      []string{"https://us-central1.example/v1"},
	}
	getModels := func(context.Context, ...string) ([]libmodelprovider.Provider, error) {
		return []libmodelprovider.Provider{other, pinned}, nil
	}
	pickPinned := func(candidates []libmodelprovider.Provider) (libmodelprovider.Provider, string, error) {
		return pinned, pinned.Backends[0], nil
	}

	selections, err := llmresolver.StreamSelections(
		context.Background(),
		llmresolver.Request{ModelNames: []string{"gemini-test"}},
		getModels,
		pickPinned,
	)
	require.NoError(t, err)
	require.Len(t, selections, 2)
	require.Equal(t, "https://global.example/v1", selections[0].Backend, "the policy's pick leads")
	require.Equal(t, "https://us-central1.example/v1", selections[1].Backend, "the remaining backend follows as the failover alternate")
}

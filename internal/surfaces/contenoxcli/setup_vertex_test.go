package contenoxcli

import (
	"context"
	"testing"

	"github.com/contenox/beam/internal/models/backendservice"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestUnit_SetupProviders_IncludeVertex asserts the setup wizard offers vertex-google with the correct auth/URL requirements.
func TestUnit_SetupProviders_IncludeVertex(t *testing.T) {
	var vertex *setupProvider
	for i := range setupProviders {
		if setupProviders[i].key == "vertex-google" {
			vertex = &setupProviders[i]
		}
	}
	require.NotNil(t, vertex, "vertex-google must be a setup wizard option")
	assert.True(t, vertex.needsBaseURL, "vertex needs an account-specific endpoint URL")
	assert.False(t, vertex.needsAPIKey, "vertex authenticates via gcloud ADC, not an API key")
	assert.NotEmpty(t, vertex.defaultModel)
}

// TestUnit_RegisterSetupBackend_VertexBaseURLAndRewire asserts registerSetupBackend persists the Vertex endpoint and rewires it to a new URL on re-run, without duplicating the backend.
func TestUnit_RegisterSetupBackend_VertexBaseURLAndRewire(t *testing.T) {
	ctx := context.Background()
	db := envSetupTestDB(t)
	svc := backendservice.New(db)

	url1 := "https://us-central1-aiplatform.googleapis.com/v1/projects/proj-a/locations/us-central1"
	require.NoError(t, registerSetupBackend(ctx, db, "vertex-google", "", url1))

	backends, err := svc.List(ctx, nil, 10)
	require.NoError(t, err)
	require.Len(t, backends, 1)
	assert.Equal(t, "vertex-google", backends[0].Type)
	assert.Equal(t, url1, backends[0].BaseURL)

	url2 := "https://europe-west4-aiplatform.googleapis.com/v1/projects/proj-b/locations/europe-west4"
	require.NoError(t, registerSetupBackend(ctx, db, "vertex-google", "", url2))

	backends, err = svc.List(ctx, nil, 10)
	require.NoError(t, err)
	require.Len(t, backends, 1, "re-running setup must not create a duplicate backend")
	assert.Equal(t, url2, backends[0].BaseURL, "the endpoint must be rewired to the new URL")
}

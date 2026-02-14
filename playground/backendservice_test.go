package playground_test

import (
	"testing"
	"time"

	"github.com/contenox/vibe/playground"
	"github.com/contenox/vibe/runtimetypes"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSystem_BackendService(t *testing.T) {
	ctx := t.Context()

	// Create a new playground instance
	p := playground.New()
	backends, err := p.WithPostgresTestContainer(ctx).GetBackendService()
	if err != nil {
		t.Fatal(err)
	}
	// Clean up the playground after the test
	defer p.CleanUp()

	t.Run("CRUD", func(t *testing.T) {
		// 1. CREATE: Valid backend
		validBackend := &runtimetypes.Backend{
			Name:    "test-backend",
			BaseURL: "http://localhost:11434",
			Type:    "ollama",
		}

		// Capture creation time for later validation
		createdAt := time.Now().UTC()
		err = backends.Create(ctx, validBackend)
		require.NoError(t, err)
		require.NotEmpty(t, validBackend.ID, "ID should be generated")
		require.WithinDuration(t, createdAt, validBackend.CreatedAt, 2*time.Second, "CreatedAt should be recent")

		// 2. GET: Retrieve created backend
		retrieved, err := backends.Get(ctx, validBackend.ID)
		require.NoError(t, err)
		assert.Equal(t, validBackend.ID, retrieved.ID)
		assert.Equal(t, validBackend.Name, retrieved.Name)
		assert.Equal(t, validBackend.BaseURL, retrieved.BaseURL)
		assert.Equal(t, validBackend.Type, retrieved.Type)
		assert.WithinDuration(t, validBackend.CreatedAt, retrieved.CreatedAt, time.Millisecond, "CreatedAt should almost match")

		// 3. UPDATE: Modify backend properties
		updatedName := "updated-backend"
		validBackend.Name = updatedName
		validBackend.BaseURL = "http://new-localhost:11434"
		err = backends.Update(ctx, validBackend)
		require.NoError(t, err)

		// Verify update via GET
		updated, err := backends.Get(ctx, validBackend.ID)
		require.NoError(t, err)
		assert.Equal(t, updatedName, updated.Name)
		assert.Equal(t, "http://new-localhost:11434", updated.BaseURL)

		// 4. LIST: Check pagination and ordering
		listed, err := backends.List(ctx, nil, 10)
		require.NoError(t, err)
		require.Len(t, listed, 1)
		assert.Equal(t, validBackend.ID, listed[0].ID)

		// Test pagination with cursor
		cursor := validBackend.CreatedAt
		emptyList, err := backends.List(ctx, &cursor, 10)
		require.NoError(t, err)
		assert.Empty(t, emptyList, "List after cursor should be empty")

		// 5. DELETE: Remove backend
		err = backends.Delete(ctx, validBackend.ID)
		require.NoError(t, err)

		// Verify deletion via GET
		_, err = backends.Get(ctx, validBackend.ID)
		assert.Error(t, err, "Should return error for deleted backend")
	})
}

package playground_test

import (
	"testing"
	"time"

	"github.com/contenox/vibe/apiframework"
	"github.com/contenox/vibe/playground"
	"github.com/contenox/vibe/runtimetypes"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSystem_ModelService(t *testing.T) {
	ctx := t.Context()

	// Create playground with required dependencies
	p := playground.New()
	p = p.WithPostgresTestContainer(ctx)
	p = p.WithDefaultEmbeddingsModel("test-embed-model", "test-provider", 1024) // Sets immutable model name
	modelService, err := p.GetModelService()
	require.NoError(t, err)
	defer p.CleanUp()

	t.Run("CRUD", func(t *testing.T) {
		// 1. CREATE: Valid model (non-immutable)
		validModel := &runtimetypes.Model{
			Model:         "test-model",
			ContextLength: 2048,
			CanChat:       true,
			CanPrompt:     true,
			CanEmbed:      true,
			CanStream:     true,
		}

		createdAt := time.Now().UTC()
		err = modelService.Append(ctx, validModel)
		require.NoError(t, err)
		require.NotEmpty(t, validModel.ID, "ID should be generated")
		require.WithinDuration(t, createdAt, validModel.CreatedAt, 2*time.Second, "CreatedAt should be recent")

		// 2. GET: Verify creation
		listed, err := modelService.List(ctx, nil, 10)
		require.NoError(t, err)
		require.Len(t, listed, 1)
		assert.Equal(t, validModel.ID, listed[0].ID)
		assert.Equal(t, validModel.Model, listed[0].Model)
		assert.Equal(t, validModel.ContextLength, listed[0].ContextLength)
		assert.True(t, listed[0].CanChat)
		assert.True(t, listed[0].CanEmbed)
		assert.WithinDuration(t, validModel.CreatedAt, listed[0].CreatedAt, time.Millisecond, "CreatedAt should match")

		// 3. UPDATE: Modify model properties
		updatedModel := *validModel
		updatedModel.CanStream = false
		updatedModel.CanPrompt = false
		err = modelService.Update(ctx, &updatedModel)
		require.NoError(t, err)

		// Verify update
		updated, err := modelService.List(ctx, nil, 10)
		require.NoError(t, err)
		require.Len(t, updated, 1)
		assert.False(t, updated[0].CanStream)
		assert.False(t, updated[0].CanPrompt)
		assert.True(t, updated[0].CanChat) // Original capability preserved

		// 4. LIST: Pagination test with two models
		// Create second model for pagination test
		model2 := &runtimetypes.Model{
			Model:         "second-model",
			ContextLength: 4096,
			CanEmbed:      true,
		}
		err = modelService.Append(ctx, model2)
		require.NoError(t, err)

		// First page: most recent model (model2)
		page1, err := modelService.List(ctx, nil, 1)
		require.NoError(t, err)
		require.Len(t, page1, 1)
		assert.Equal(t, model2.ID, page1[0].ID)

		// Second page: next model (original model)
		page2, err := modelService.List(ctx, &page1[0].CreatedAt, 1)
		require.NoError(t, err)
		require.Len(t, page2, 1)
		assert.Equal(t, validModel.ID, page2[0].ID)

		// Third page: should be empty
		page3, err := modelService.List(ctx, &page2[0].CreatedAt, 1)
		require.NoError(t, err)
		assert.Empty(t, page3, "Pagination after last item should be empty")

		// 5. DELETE: Remove non-immutable model
		err = modelService.Delete(ctx, validModel.Model)
		require.NoError(t, err)

		// Verify deletion
		afterDelete, err := modelService.List(ctx, nil, 10)
		require.NoError(t, err)
		require.Len(t, afterDelete, 1)
		assert.Equal(t, model2.ID, afterDelete[0].ID)

		// 6. IMMUTABLE MODEL TEST
		// Create immutable model
		immutableModel := &runtimetypes.Model{
			Model:         "test-embed-model", // Matches playground's immutable name
			ContextLength: 512,
			CanEmbed:      true,
		}
		err = modelService.Append(ctx, immutableModel)
		require.NoError(t, err)

		// Attempt to delete immutable model
		err = modelService.Delete(ctx, "test-embed-model")
		require.Error(t, err)
		assert.ErrorIs(t, err, apiframework.ErrImmutableModel, "Should block deletion of immutable model")

		// Verify immutable model still exists
		immutableCheck, err := modelService.List(ctx, nil, 10)
		require.NoError(t, err)
		require.Len(t, immutableCheck, 2) // model2 + immutable model
		assert.Contains(t, []string{immutableCheck[0].Model, immutableCheck[1].Model}, "test-embed-model")

		// 7. VALIDATION TESTS
		// Invalid model: missing model name
		invalidModel := &runtimetypes.Model{
			ContextLength: 2048,
			CanChat:       true,
		}
		err = modelService.Append(ctx, invalidModel)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "model name is required")

		// Invalid model: missing context length
		invalidModel2 := &runtimetypes.Model{
			Model:   "invalid-model",
			CanChat: true,
		}
		err = modelService.Append(ctx, invalidModel2)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "context length is required")

		// Invalid model: no capabilities
		invalidModel3 := &runtimetypes.Model{
			Model:         "invalid-model",
			ContextLength: 2048,
		}
		err = modelService.Append(ctx, invalidModel3)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "capabilities are required")

		// Invalid update: missing ID
		invalidUpdate := &runtimetypes.Model{
			Model:         "test-model",
			ContextLength: 4096,
		}
		err = modelService.Update(ctx, invalidUpdate)
		require.Error(t, err)

		// Invalid update: missing model name
		invalidUpdate2 := *validModel
		invalidUpdate2.Model = ""
		err = modelService.Update(ctx, &invalidUpdate2)
		require.Error(t, err)
	})
}

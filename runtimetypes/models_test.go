package runtimetypes_test

import (
	"fmt"
	"testing"
	"time"

	libdb "github.com/contenox/vibe/libdbexec"
	"github.com/contenox/vibe/runtimetypes"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestUnit_Models_AppendAndGetAllModels(t *testing.T) {
	ctx, s := runtimetypes.SetupStore(t)
	limit := 100 // Use a large limit to fetch all models

	models, err := s.ListModels(ctx, nil, limit)
	require.NoError(t, err)
	require.Empty(t, models)

	// Append a new model with capability fields
	model := &runtimetypes.Model{
		ID:            uuid.New().String(),
		Model:         "test-model",
		ContextLength: 4096,
		CanChat:       true,
		CanEmbed:      false,
		CanPrompt:     true,
		CanStream:     false,
	}
	err = s.AppendModel(ctx, model)
	require.NoError(t, err)
	require.NotEmpty(t, model.CreatedAt)
	require.NotEmpty(t, model.UpdatedAt)

	models, err = s.ListModels(ctx, nil, limit)
	require.NoError(t, err)
	require.Len(t, models, 1)
	require.Equal(t, "test-model", models[0].Model)
	require.Equal(t, 4096, models[0].ContextLength)
	require.True(t, models[0].CanChat)
	require.False(t, models[0].CanEmbed)
	require.True(t, models[0].CanPrompt)
	require.False(t, models[0].CanStream)
	require.WithinDuration(t, model.CreatedAt, models[0].CreatedAt, time.Second)
	require.WithinDuration(t, model.UpdatedAt, models[0].UpdatedAt, time.Second)
}

func TestUnit_Models_DeleteModel(t *testing.T) {
	ctx, s := runtimetypes.SetupStore(t)
	limit := 100

	model := &runtimetypes.Model{
		ID:            uuid.New().String(),
		Model:         "model-to-delete",
		ContextLength: 2048,
		CanChat:       false,
		CanEmbed:      true,
		CanPrompt:     false,
		CanStream:     true,
	}
	err := s.AppendModel(ctx, model)
	require.NoError(t, err)

	err = s.DeleteModel(ctx, "model-to-delete")
	require.NoError(t, err)

	models, err := s.ListModels(ctx, nil, limit)
	require.NoError(t, err)
	require.Empty(t, models)
}

func TestUnit_Models_DeleteNonExistentModel(t *testing.T) {
	ctx, s := runtimetypes.SetupStore(t)

	err := s.DeleteModel(ctx, "non-existent-model")
	require.ErrorIs(t, err, libdb.ErrNotFound)
}

func TestUnit_Models_GetAllModelsOrder(t *testing.T) {
	ctx, s := runtimetypes.SetupStore(t)
	limit := 100

	model1 := &runtimetypes.Model{
		ID:            uuid.New().String(),
		Model:         "model1",
		ContextLength: 1024,
		CanChat:       true,
		CanEmbed:      true,
		CanPrompt:     true,
		CanStream:     true,
	}
	err := s.AppendModel(ctx, model1)
	require.NoError(t, err)

	model2 := &runtimetypes.Model{
		ID:            uuid.New().String(),
		Model:         "model2",
		ContextLength: 8192,
		CanChat:       false,
		CanEmbed:      false,
		CanPrompt:     false,
		CanStream:     false,
	}
	err = s.AppendModel(ctx, model2)
	require.NoError(t, err)

	models, err := s.ListModels(ctx, nil, limit)
	require.NoError(t, err)
	require.Len(t, models, 2)
	require.Equal(t, "model2", models[0].Model)
	require.Equal(t, 8192, models[0].ContextLength)
	require.False(t, models[0].CanChat)
	require.False(t, models[0].CanEmbed)
	require.False(t, models[0].CanPrompt)
	require.False(t, models[0].CanStream)
	require.Equal(t, "model1", models[1].Model)
	require.Equal(t, 1024, models[1].ContextLength)
	require.True(t, models[1].CanChat)
	require.True(t, models[1].CanEmbed)
	require.True(t, models[1].CanPrompt)
	require.True(t, models[1].CanStream)
	require.True(t, models[0].CreatedAt.After(models[1].CreatedAt))
}

func TestUnit_Models_AppendDuplicateModel(t *testing.T) {
	ctx, s := runtimetypes.SetupStore(t)

	model := &runtimetypes.Model{
		Model:         "duplicate-model",
		ContextLength: 4096,
		CanChat:       true,
		CanEmbed:      false,
		CanPrompt:     true,
		CanStream:     false,
	}
	err := s.AppendModel(ctx, model)
	require.NoError(t, err)

	// Attempt to append duplicate model with same capabilities
	duplicate := &runtimetypes.Model{
		Model:         "duplicate-model",
		ContextLength: 4096, // Same capabilities
		CanChat:       true,
		CanEmbed:      false,
		CanPrompt:     true,
		CanStream:     false,
	}
	err = s.AppendModel(ctx, duplicate)
	require.Error(t, err)
}

func TestUnit_Models_GetModelByName(t *testing.T) {
	ctx, s := runtimetypes.SetupStore(t)

	model := &runtimetypes.Model{
		ID:            uuid.New().String(),
		Model:         "model-to-get",
		ContextLength: 2048,
		CanChat:       true,
		CanEmbed:      false,
		CanPrompt:     true,
		CanStream:     false,
	}
	err := s.AppendModel(ctx, model)
	require.NoError(t, err)

	foundModel, err := s.GetModelByName(ctx, "model-to-get")
	require.NoError(t, err)
	require.Equal(t, model.ID, foundModel.ID)
	require.Equal(t, model.Model, foundModel.Model)
	require.Equal(t, 2048, foundModel.ContextLength)
	require.True(t, foundModel.CanChat)
	require.False(t, foundModel.CanEmbed)
	require.True(t, foundModel.CanPrompt)
	require.False(t, foundModel.CanStream)
	require.WithinDuration(t, model.CreatedAt, foundModel.CreatedAt, time.Second)
	require.WithinDuration(t, model.UpdatedAt, foundModel.UpdatedAt, time.Second)
}

func TestUnit_Models_ListHandlesPagination(t *testing.T) {
	ctx, s := runtimetypes.SetupStore(t)

	// Create 5 models with capability fields
	var createdModels []*runtimetypes.Model
	for i := range 5 {
		model := &runtimetypes.Model{
			ID:            uuid.New().String(),
			Model:         fmt.Sprintf("model%d", i),
			ContextLength: 1024 + i*1024, // Vary context length
			CanChat:       i%2 == 0,
			CanEmbed:      i%3 == 0,
			CanPrompt:     i%4 == 0,
			CanStream:     i%5 == 0,
		}
		err := s.AppendModel(ctx, model)
		require.NoError(t, err)
		createdModels = append(createdModels, model)
	}

	// Paginate through the results with a limit of 2
	var receivedModels []*runtimetypes.Model
	var lastCursor *time.Time
	limit := 2

	// Fetch first page
	page1, err := s.ListModels(ctx, lastCursor, limit)
	require.NoError(t, err)
	require.Len(t, page1, 2)
	receivedModels = append(receivedModels, page1...)

	lastCursor = &page1[len(page1)-1].CreatedAt

	// Fetch second page
	page2, err := s.ListModels(ctx, lastCursor, limit)
	require.NoError(t, err)
	require.Len(t, page2, 2)
	receivedModels = append(receivedModels, page2...)

	lastCursor = &page2[len(page2)-1].CreatedAt

	// Fetch third page (the last one)
	page3, err := s.ListModels(ctx, lastCursor, limit)
	require.NoError(t, err)
	require.Len(t, page3, 1)
	receivedModels = append(receivedModels, page3...)

	// Fetch a fourth page, which should be empty
	page4, err := s.ListModels(ctx, &page3[0].CreatedAt, limit)
	require.NoError(t, err)
	require.Empty(t, page4)

	// Verify all models were retrieved in the correct order
	require.Len(t, receivedModels, 5)

	// Check capability fields for each retrieved model
	for i, received := range receivedModels {
		expected := createdModels[4-i] // Reverse order (newest first)
		require.Equal(t, expected.Model, received.Model)
		require.Equal(t, expected.ContextLength, received.ContextLength)
		require.Equal(t, expected.CanChat, received.CanChat)
		require.Equal(t, expected.CanEmbed, received.CanEmbed)
		require.Equal(t, expected.CanPrompt, received.CanPrompt)
		require.Equal(t, expected.CanStream, received.CanStream)
	}
}

// New test for context length validation
func TestUnit_Models_InvalidContextLength(t *testing.T) {
	ctx, s := runtimetypes.SetupStore(t)

	// Test zero context length
	model := &runtimetypes.Model{
		ID:            uuid.New().String(),
		Model:         "invalid-model",
		ContextLength: 0, // Invalid value
	}
	err := s.AppendModel(ctx, model)
	require.Error(t, err)
	require.Contains(t, err.Error(), "context length cannot be zero")

	// Test negative context length
	model.ContextLength = -100
	err = s.AppendModel(ctx, model)
	require.Error(t, err)
	require.Contains(t, err.Error(), "context length cannot be zero")
}

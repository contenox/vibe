package runtimetypes_test

import (
	"testing"
	"time"

	libdb "github.com/contenox/contenox/libdbexec"
	"github.com/contenox/contenox/runtimetypes"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestUnit_ModelRegistry_CreatesAndFetchesByID(t *testing.T) {
	ctx, s := runtimetypes.SetupStore(t)

	e := &runtimetypes.ModelRegistryEntry{
		ID:        uuid.NewString(),
		Name:      "test-model",
		SourceURL: "https://huggingface.co/test/model.gguf",
		SizeBytes: 500_000_000,
	}

	require.NoError(t, s.CreateModelRegistryEntry(ctx, e))
	require.NotEmpty(t, e.ID)

	got, err := s.GetModelRegistryEntry(ctx, e.ID)
	require.NoError(t, err)
	require.Equal(t, e.Name, got.Name)
	require.Equal(t, e.SourceURL, got.SourceURL)
	require.Equal(t, e.SizeBytes, got.SizeBytes)
	require.WithinDuration(t, e.CreatedAt, got.CreatedAt, time.Second)
}

func TestUnit_ModelRegistry_UpdatesFieldsCorrectly(t *testing.T) {
	ctx, s := runtimetypes.SetupStore(t)

	e := &runtimetypes.ModelRegistryEntry{
		ID:        uuid.NewString(),
		Name:      "update-model",
		SourceURL: "https://huggingface.co/original/model.gguf",
	}
	require.NoError(t, s.CreateModelRegistryEntry(ctx, e))

	e.SourceURL = "https://huggingface.co/updated/model.gguf"
	e.SizeBytes = 1_000_000_000
	require.NoError(t, s.UpdateModelRegistryEntry(ctx, e))

	got, err := s.GetModelRegistryEntry(ctx, e.ID)
	require.NoError(t, err)
	require.Equal(t, "https://huggingface.co/updated/model.gguf", got.SourceURL)
	require.Equal(t, int64(1_000_000_000), got.SizeBytes)
	require.True(t, got.UpdatedAt.After(got.CreatedAt) || got.UpdatedAt.Equal(got.CreatedAt))
}

func TestUnit_ModelRegistry_DeletesSuccessfully(t *testing.T) {
	ctx, s := runtimetypes.SetupStore(t)

	e := &runtimetypes.ModelRegistryEntry{
		ID:        uuid.NewString(),
		Name:      "delete-model",
		SourceURL: "https://huggingface.co/del/model.gguf",
	}
	require.NoError(t, s.CreateModelRegistryEntry(ctx, e))
	require.NoError(t, s.DeleteModelRegistryEntry(ctx, e.ID))

	_, err := s.GetModelRegistryEntry(ctx, e.ID)
	require.ErrorIs(t, err, libdb.ErrNotFound)
}

func TestUnit_ModelRegistry_ListHandlesPagination(t *testing.T) {
	ctx, s := runtimetypes.SetupStore(t)

	for i := range 5 {
		e := &runtimetypes.ModelRegistryEntry{
			ID:        uuid.NewString(),
			Name:      "paginate-model-" + string(rune('a'+i)),
			SourceURL: "https://huggingface.co/test/model.gguf",
		}
		require.NoError(t, s.CreateModelRegistryEntry(ctx, e))
	}

	page1, err := s.ListModelRegistryEntries(ctx, nil, 2)
	require.NoError(t, err)
	require.Len(t, page1, 2)

	cursor := page1[len(page1)-1].CreatedAt
	page2, err := s.ListModelRegistryEntries(ctx, &cursor, 2)
	require.NoError(t, err)
	require.Len(t, page2, 2)

	cursor2 := page2[len(page2)-1].CreatedAt
	page3, err := s.ListModelRegistryEntries(ctx, &cursor2, 2)
	require.NoError(t, err)
	require.Len(t, page3, 1)
}

func TestUnit_ModelRegistry_FetchesByName(t *testing.T) {
	ctx, s := runtimetypes.SetupStore(t)

	e := &runtimetypes.ModelRegistryEntry{
		ID:        uuid.NewString(),
		Name:      "named-model",
		SourceURL: "https://huggingface.co/named/model.gguf",
	}
	require.NoError(t, s.CreateModelRegistryEntry(ctx, e))

	got, err := s.GetModelRegistryEntryByName(ctx, "named-model")
	require.NoError(t, err)
	require.Equal(t, e.ID, got.ID)
}

func TestUnit_ModelRegistry_GetNonexistentReturnsNotFound(t *testing.T) {
	ctx, s := runtimetypes.SetupStore(t)

	_, err := s.GetModelRegistryEntry(ctx, uuid.NewString())
	require.ErrorIs(t, err, libdb.ErrNotFound)

	_, err = s.GetModelRegistryEntryByName(ctx, "does-not-exist")
	require.ErrorIs(t, err, libdb.ErrNotFound)
}

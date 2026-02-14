package runtimetypes_test

import (
	"fmt"
	"strconv"
	"testing"
	"time"

	libdb "github.com/contenox/vibe/libdbexec"
	"github.com/contenox/vibe/runtimetypes"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestUnit_Backend_CreatesAndFetchesByID(t *testing.T) {
	ctx, s := runtimetypes.SetupStore(t)

	backend := &runtimetypes.Backend{
		ID:      uuid.NewString(),
		Name:    "TestBackend",
		BaseURL: "http://localhost:8080",
		Type:    "ollama",
	}

	// Create the backend.
	err := s.CreateBackend(ctx, backend)
	require.NoError(t, err)
	require.NotEmpty(t, backend.ID)

	// Retrieve the backend by ID.
	got, err := s.GetBackend(ctx, backend.ID)
	require.NoError(t, err)
	require.Equal(t, backend.Name, got.Name)
	require.Equal(t, backend.BaseURL, got.BaseURL)
	require.Equal(t, backend.Type, got.Type)
	require.WithinDuration(t, backend.CreatedAt, got.CreatedAt, time.Second)
	require.WithinDuration(t, backend.UpdatedAt, got.UpdatedAt, time.Second)
}

func TestUnit_Backend_UpdatesFieldsCorrectly(t *testing.T) {
	ctx, s := runtimetypes.SetupStore(t)

	backend := &runtimetypes.Backend{
		ID:      uuid.NewString(),
		Name:    "InitialBackend",
		BaseURL: "http://initial.url",
		Type:    "ollama",
	}

	// Create the backend.
	err := s.CreateBackend(ctx, backend)
	require.NoError(t, err)

	// Modify some fields.
	backend.Name = "UpdatedBackend"
	backend.BaseURL = "http://updated.url"
	backend.Type = "OpenAI"

	// Update the backend.
	err = s.UpdateBackend(ctx, backend)
	require.NoError(t, err)

	// Retrieve and verify the update.
	got, err := s.GetBackend(ctx, backend.ID)
	require.NoError(t, err)
	require.Equal(t, "UpdatedBackend", got.Name)
	require.Equal(t, "http://updated.url", got.BaseURL)
	require.Equal(t, "OpenAI", got.Type)
	require.True(t, got.UpdatedAt.After(got.CreatedAt))
}

func TestUnit_Backend_DeletesSuccessfully(t *testing.T) {
	ctx, s := runtimetypes.SetupStore(t)

	backend := &runtimetypes.Backend{
		ID:      uuid.NewString(),
		Name:    "ToDelete",
		BaseURL: "http://delete.me",
		Type:    "ollama",
	}

	// Create the backend.
	err := s.CreateBackend(ctx, backend)
	require.NoError(t, err)

	// Delete the backend.
	err = s.DeleteBackend(ctx, backend.ID)
	require.NoError(t, err)

	// Attempt to retrieve the deleted backend; expect an error.
	_, err = s.GetBackend(ctx, backend.ID)
	require.ErrorIs(t, err, libdb.ErrNotFound)
}

func TestUnit_Backend_ListHandlesPagination(t *testing.T) {
	ctx, s := runtimetypes.SetupStore(t)

	// Create 5 backends with a small delay to ensure distinct creation times.
	var backends []*runtimetypes.Backend
	for i := range 5 {
		backend := &runtimetypes.Backend{
			ID:      uuid.NewString(),
			Name:    fmt.Sprintf("Backend%d", i),
			BaseURL: "http://example.com" + strconv.Itoa(i),
			Type:    "ollama",
		}
		err := s.CreateBackend(ctx, backend)
		require.NoError(t, err)
		backends = append(backends, backend)
	}

	// Paginate through the results with a limit of 2.
	var receivedBackends []*runtimetypes.Backend
	var lastCursor *time.Time
	limit := 2

	// Fetch first page
	page1, err := s.ListBackends(ctx, lastCursor, limit)
	require.NoError(t, err)
	require.Len(t, page1, 2)
	receivedBackends = append(receivedBackends, page1...)

	// The cursor for the next page is the creation time of the last item
	lastCursor = &page1[len(page1)-1].CreatedAt

	// Fetch second page
	page2, err := s.ListBackends(ctx, lastCursor, limit)
	require.NoError(t, err)
	require.Len(t, page2, 2)
	receivedBackends = append(receivedBackends, page2...)

	lastCursor = &page2[len(page2)-1].CreatedAt

	// Fetch third page (the last one)
	page3, err := s.ListBackends(ctx, lastCursor, limit)
	require.NoError(t, err)
	require.Len(t, page3, 1)
	receivedBackends = append(receivedBackends, page3...)

	// Fetch a fourth page, which should be empty
	page4, err := s.ListBackends(ctx, &page3[0].CreatedAt, limit)
	require.NoError(t, err)
	require.Empty(t, page4)

	// Verify all backends were retrieved in the correct order.
	require.Len(t, receivedBackends, 5)

	// The order is newest to oldest, so the last created backend should be first.
	require.Equal(t, backends[4].ID, receivedBackends[0].ID)
	require.Equal(t, backends[3].ID, receivedBackends[1].ID)
	require.Equal(t, backends[2].ID, receivedBackends[2].ID)
	require.Equal(t, backends[1].ID, receivedBackends[3].ID)
	require.Equal(t, backends[0].ID, receivedBackends[4].ID)
}

func TestUnit_Backend_FetchesByName(t *testing.T) {
	ctx, s := runtimetypes.SetupStore(t)

	backend := &runtimetypes.Backend{
		ID:      uuid.NewString(),
		Name:    "UniqueBackend",
		BaseURL: "http://unique",
		Type:    "ollama",
	}

	// Create the backend.
	err := s.CreateBackend(ctx, backend)
	require.NoError(t, err)

	// Retrieve the backend by name.
	got, err := s.GetBackendByName(ctx, "UniqueBackend")
	require.NoError(t, err)
	require.Equal(t, backend.ID, got.ID)
}

func TestUnit_Backend_GetNonexistentReturnsNotFound(t *testing.T) {
	ctx, s := runtimetypes.SetupStore(t)

	// Test retrieval by a non-existent ID.
	_, err := s.GetBackend(ctx, uuid.NewString())
	require.ErrorIs(t, err, libdb.ErrNotFound)

	// Test retrieval by a non-existent name.
	_, err = s.GetBackendByName(ctx, "non-existent-name")
	require.ErrorIs(t, err, libdb.ErrNotFound)
}

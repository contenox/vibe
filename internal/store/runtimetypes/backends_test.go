package runtimetypes_test

import (
	"fmt"
	"strconv"
	"testing"
	"time"

	libdb "github.com/contenox/beam/internal/libdbexec"
	"github.com/contenox/beam/internal/store/runtimetypes"
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

	err := s.CreateBackend(ctx, backend)
	require.NoError(t, err)
	require.NotEmpty(t, backend.ID)

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

	err := s.CreateBackend(ctx, backend)
	require.NoError(t, err)

	backend.Name = "UpdatedBackend"
	backend.BaseURL = "http://updated.url"
	backend.Type = "OpenAI"

	err = s.UpdateBackend(ctx, backend)
	require.NoError(t, err)

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

	err := s.CreateBackend(ctx, backend)
	require.NoError(t, err)

	err = s.DeleteBackend(ctx, backend.ID)
	require.NoError(t, err)

	_, err = s.GetBackend(ctx, backend.ID)
	require.ErrorIs(t, err, libdb.ErrNotFound)
}

func TestUnit_Backend_ListHandlesPagination(t *testing.T) {
	ctx, s := runtimetypes.SetupStore(t)

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

	var receivedBackends []*runtimetypes.Backend
	var lastCursor *time.Time
	limit := 2

	page1, err := s.ListBackends(ctx, lastCursor, limit)
	require.NoError(t, err)
	require.Len(t, page1, 2)
	receivedBackends = append(receivedBackends, page1...)

	lastCursor = &page1[len(page1)-1].CreatedAt

	page2, err := s.ListBackends(ctx, lastCursor, limit)
	require.NoError(t, err)
	require.Len(t, page2, 2)
	receivedBackends = append(receivedBackends, page2...)

	lastCursor = &page2[len(page2)-1].CreatedAt

	page3, err := s.ListBackends(ctx, lastCursor, limit)
	require.NoError(t, err)
	require.Len(t, page3, 1)
	receivedBackends = append(receivedBackends, page3...)

	page4, err := s.ListBackends(ctx, &page3[0].CreatedAt, limit)
	require.NoError(t, err)
	require.Empty(t, page4)

	require.Len(t, receivedBackends, 5)

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

	err := s.CreateBackend(ctx, backend)
	require.NoError(t, err)

	got, err := s.GetBackendByName(ctx, "UniqueBackend")
	require.NoError(t, err)
	require.Equal(t, backend.ID, got.ID)
}

func TestUnit_Backend_GetNonexistentReturnsNotFound(t *testing.T) {
	ctx, s := runtimetypes.SetupStore(t)

	_, err := s.GetBackend(ctx, uuid.NewString())
	require.ErrorIs(t, err, libdb.ErrNotFound)

	_, err = s.GetBackendByName(ctx, "non-existent-name")
	require.ErrorIs(t, err, libdb.ErrNotFound)
}

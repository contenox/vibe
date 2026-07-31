package terminalstore_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	libdb "github.com/contenox/contenox/internal/libdbexec"
	"github.com/contenox/contenox/internal/services/terminalstore"
	"github.com/contenox/contenox/internal/store/runtimetypes"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func setupStore(t *testing.T, workspaceID string) (context.Context, libdb.DBManager, terminalstore.Store) {
	t.Helper()
	ctx := context.Background()
	db, err := libdb.NewSQLiteDBManager(ctx, filepath.Join(t.TempDir(), "terminal.db"), "")
	require.NoError(t, err)
	require.NoError(t, terminalstore.InitSchema(ctx, db.WithoutTransaction()))
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	return ctx, db, terminalstore.New(db.WithoutTransaction(), workspaceID)
}

func TestStore_InsertGetDelete(t *testing.T) {
	ctx, _, st := setupStore(t, "ws-1")

	id := uuid.NewString()
	now := time.Now().UTC()
	s := &terminalstore.Session{
		ID:             id,
		Principal:      "user-a",
		CWD:            "/tmp",
		Shell:          "/bin/bash",
		Cols:           80,
		Rows:           24,
		Status:         terminalstore.SessionStatusActive,
		NodeInstanceID: "node-1",
		CreatedAt:      now,
		UpdatedAt:      now,
	}
	require.NoError(t, st.Insert(ctx, s))

	got, err := st.GetByID(ctx, id)
	require.NoError(t, err)
	require.Equal(t, id, got.ID)
	require.Equal(t, "user-a", got.Principal)
	require.Equal(t, "ws-1", got.WorkspaceID)

	got2, err := st.GetByIDAndPrincipal(ctx, id, "user-a")
	require.NoError(t, err)
	require.Equal(t, id, got2.ID)

	_, err = st.GetByIDAndPrincipal(ctx, id, "other")
	require.ErrorIs(t, err, terminalstore.ErrNotFound)

	require.NoError(t, st.Delete(ctx, id))
	_, err = st.GetByID(ctx, id)
	require.ErrorIs(t, err, terminalstore.ErrNotFound)
}

func TestStore_ListByPrincipalPagination(t *testing.T) {
	ctx, _, st := setupStore(t, "ws-1")

	base := time.Now().UTC().Add(-1 * time.Hour)
	for i := 0; i < 5; i++ {
		ts := base.Add(time.Duration(i) * time.Minute)
		require.NoError(t, st.Insert(ctx, &terminalstore.Session{
			ID:             uuid.NewString(),
			Principal:      "p1",
			CWD:            "/tmp",
			Shell:          "/bin/bash",
			Cols:           80,
			Rows:           24,
			Status:         terminalstore.SessionStatusActive,
			NodeInstanceID: "node-1",
			CreatedAt:      ts,
			UpdatedAt:      ts,
		}))
	}

	page1, err := st.ListByPrincipal(ctx, "p1", nil, 2)
	require.NoError(t, err)
	require.Len(t, page1, 2)
	require.True(t, page1[0].CreatedAt.After(page1[1].CreatedAt) || page1[0].CreatedAt.Equal(page1[1].CreatedAt))

	cursor := page1[1].CreatedAt
	page2, err := st.ListByPrincipal(ctx, "p1", &cursor, 10)
	require.NoError(t, err)
	require.NotEmpty(t, page2)
}

func TestStore_ListLimitExceeded(t *testing.T) {
	ctx, _, st := setupStore(t, "")
	_, err := st.ListByPrincipal(ctx, "x", nil, runtimetypes.MAXLIMIT+1)
	require.ErrorIs(t, err, runtimetypes.ErrLimitParamExceeded)
}

func TestStore_DeleteByNodeInstanceID(t *testing.T) {
	ctx, _, st := setupStore(t, "")

	id := uuid.NewString()
	now := time.Now().UTC()
	require.NoError(t, st.Insert(ctx, &terminalstore.Session{
		ID: id, Principal: "u", CWD: "/tmp", Shell: "/bin/bash", Cols: 80, Rows: 24,
		Status: terminalstore.SessionStatusActive, NodeInstanceID: "n99",
		CreatedAt: now, UpdatedAt: now,
	}))
	require.NoError(t, st.DeleteByNodeInstanceID(ctx, "n99"))
	_, err := st.GetByID(ctx, id)
	require.ErrorIs(t, err, terminalstore.ErrNotFound)
}

func TestStore_UpdateGeometry(t *testing.T) {
	ctx, _, st := setupStore(t, "")

	id := uuid.NewString()
	now := time.Now().UTC()
	require.NoError(t, st.Insert(ctx, &terminalstore.Session{
		ID: id, Principal: "u", CWD: "/tmp", Shell: "/bin/bash", Cols: 80, Rows: 24,
		Status: terminalstore.SessionStatusActive, NodeInstanceID: "n1",
		CreatedAt: now, UpdatedAt: now,
	}))
	require.NoError(t, st.UpdateGeometry(ctx, id, 100, 40))
	got, err := st.GetByID(ctx, id)
	require.NoError(t, err)
	require.Equal(t, 100, got.Cols)
	require.Equal(t, 40, got.Rows)
}

func TestStore_WorkspaceIsolation(t *testing.T) {
	ctx, db, stA := setupStore(t, "a")
	stB := terminalstore.New(db.WithoutTransaction(), "b")

	id := uuid.NewString()
	now := time.Now().UTC()
	require.NoError(t, stA.Insert(ctx, &terminalstore.Session{
		ID: id, Principal: "u", CWD: "/tmp", Shell: "/bin/bash", Cols: 80, Rows: 24,
		Status: terminalstore.SessionStatusActive, NodeInstanceID: "n1",
		CreatedAt: now, UpdatedAt: now,
	}))
	_, err := stB.GetByID(ctx, id)
	require.ErrorIs(t, err, terminalstore.ErrNotFound)
}

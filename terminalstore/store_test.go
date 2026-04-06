package terminalstore_test

import (
	"testing"
	"time"

	"github.com/contenox/contenox/runtimetypes"
	"github.com/contenox/contenox/terminalstore"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestStore_InsertGetDelete(t *testing.T) {
	ctx, db := SetupStore(t)
	st := terminalstore.New(db.WithoutTransaction())

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
	ctx, db := SetupStore(t)
	st := terminalstore.New(db.WithoutTransaction())

	base := time.Now().UTC().Add(-1 * time.Hour)
	for i := 0; i < 5; i++ {
		id := uuid.NewString()
		ts := base.Add(time.Duration(i) * time.Minute)
		s := &terminalstore.Session{
			ID:             id,
			Principal:      "p1",
			CWD:            "/tmp",
			Shell:          "/bin/bash",
			Cols:           80,
			Rows:           24,
			Status:         terminalstore.SessionStatusActive,
			NodeInstanceID: "node-1",
			CreatedAt:      ts,
			UpdatedAt:      ts,
		}
		require.NoError(t, st.Insert(ctx, s))
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
	ctx, db := SetupStore(t)
	st := terminalstore.New(db.WithoutTransaction())
	_, err := st.ListByPrincipal(ctx, "x", nil, runtimetypes.MAXLIMIT+1)
	require.ErrorIs(t, err, runtimetypes.ErrLimitParamExceeded)
}

func TestStore_DeleteByNodeInstanceID(t *testing.T) {
	ctx, db := SetupStore(t)
	st := terminalstore.New(db.WithoutTransaction())

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
	ctx, db := SetupStore(t)
	st := terminalstore.New(db.WithoutTransaction())

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

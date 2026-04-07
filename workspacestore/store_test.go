package workspacestore_test

import (
	"testing"
	"time"

	"github.com/contenox/contenox/runtimetypes"
	"github.com/contenox/contenox/workspacestore"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestWorkspaceStore_CRUD(t *testing.T) {
	ctx, db := SetupStore(t)
	st := workspacestore.New(db.WithoutTransaction())

	id := uuid.NewString()
	now := time.Now().UTC()
	w := &workspacestore.Workspace{
		ID: id, Principal: "u1", Name: "dev", Path: "/tmp/ws",
		CreatedAt: now, UpdatedAt: now,
	}
	require.NoError(t, st.Insert(ctx, w))

	got, err := st.GetByIDAndPrincipal(ctx, id, "u1")
	require.NoError(t, err)
	require.Equal(t, "dev", got.Name)

	_, err = st.GetByIDAndPrincipal(ctx, id, "other")
	require.ErrorIs(t, err, workspacestore.ErrNotFound)

	w.Name = "prod"
	w.Path = "/tmp/ws2"
	require.NoError(t, st.Update(ctx, w))

	require.NoError(t, st.DeleteByIDAndPrincipal(ctx, id, "u1"))
	_, err = st.GetByID(ctx, id)
	require.ErrorIs(t, err, workspacestore.ErrNotFound)
}

func TestWorkspaceStore_ListPagination(t *testing.T) {
	ctx, db := SetupStore(t)
	st := workspacestore.New(db.WithoutTransaction())
	base := time.Now().UTC().Add(-time.Hour)
	for i := 0; i < 3; i++ {
		id := uuid.NewString()
		ts := base.Add(time.Duration(i) * time.Minute)
		require.NoError(t, st.Insert(ctx, &workspacestore.Workspace{
			ID: id, Principal: "p", Name: id[:8], Path: "/tmp/x",
			CreatedAt: ts, UpdatedAt: ts,
		}))
	}
	page, err := st.ListByPrincipal(ctx, "p", nil, 2)
	require.NoError(t, err)
	require.Len(t, page, 2)
}

func TestWorkspaceStore_ListLimitExceeded(t *testing.T) {
	ctx, db := SetupStore(t)
	st := workspacestore.New(db.WithoutTransaction())
	_, err := st.ListByPrincipal(ctx, "x", nil, runtimetypes.MAXLIMIT+1)
	require.ErrorIs(t, err, runtimetypes.ErrLimitParamExceeded)
}

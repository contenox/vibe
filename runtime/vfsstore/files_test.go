package vfsstore_test

import (
	"context"
	"testing"
	"time"

	libdb "github.com/contenox/contenox/libdbexec"
	"github.com/contenox/contenox/runtime/vfsstore"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// newFileID inserts a minimal vfs_files record and returns its ID.
// Required by tree tests to satisfy vfs_filestree.id → vfs_files.id FK.
func newFileID(t *testing.T, s vfsstore.Store, ctx context.Context) string {
	t.Helper()
	id := uuid.NewString()
	err := s.CreateFile(ctx, &vfsstore.File{
		ID:   id,
		Type: "text/plain",
		Meta: []byte(`{}`),
	})
	require.NoError(t, err)
	return id
}

func TestUnit_CreateAndGetFile(t *testing.T) {
	ctx, s := SetupStore(t)

	// Create the blob first to satisfy the FK constraint.
	blob := &vfsstore.Blob{
		ID:   uuid.NewString(),
		Meta: []byte(`{}`),
		Data: []byte("hello"),
	}
	require.NoError(t, s.CreateBlob(ctx, blob))

	file := &vfsstore.File{
		ID:      uuid.NewString(),
		Type:    "text/plain",
		Meta:    []byte(`{"description": "Test file"}`),
		BlobsID: blob.ID,
	}

	err := s.CreateFile(ctx, file)
	require.NoError(t, err)
	require.NotZero(t, file.CreatedAt)
	require.NotZero(t, file.UpdatedAt)

	retrieved, err := s.GetFileByID(ctx, file.ID)
	require.NoError(t, err)
	require.Equal(t, file.ID, retrieved.ID)
	require.Equal(t, file.Type, retrieved.Type)
	require.Equal(t, file.Meta, retrieved.Meta)
	require.Equal(t, file.BlobsID, retrieved.BlobsID)
	require.WithinDuration(t, file.CreatedAt, retrieved.CreatedAt, time.Second)
	require.WithinDuration(t, file.UpdatedAt, retrieved.UpdatedAt, time.Second)
}

func TestUnit_UpdateFile(t *testing.T) {
	ctx, s := SetupStore(t)

	blob := &vfsstore.Blob{ID: uuid.NewString(), Meta: []byte(`{}`), Data: []byte("v1")}
	require.NoError(t, s.CreateBlob(ctx, blob))

	file := &vfsstore.File{
		ID:      uuid.NewString(),
		Type:    "text/plain",
		Meta:    []byte(`{"description": "Old description"}`),
		BlobsID: blob.ID,
	}
	require.NoError(t, s.CreateFile(ctx, file))

	blob2 := &vfsstore.Blob{ID: uuid.NewString(), Meta: []byte(`{}`), Data: []byte("v2")}
	require.NoError(t, s.CreateBlob(ctx, blob2))

	file.Type = "application/json"
	file.Meta = []byte(`{"description": "New description"}`)
	file.BlobsID = blob2.ID
	require.NoError(t, s.UpdateFile(ctx, file))

	updated, err := s.GetFileByID(ctx, file.ID)
	require.NoError(t, err)
	require.Equal(t, "application/json", updated.Type)
	require.Equal(t, file.Meta, updated.Meta)
	require.Equal(t, blob2.ID, updated.BlobsID)
	require.True(t, updated.UpdatedAt.After(updated.CreatedAt))
}

func TestUnit_DeleteFile(t *testing.T) {
	ctx, s := SetupStore(t)

	blob := &vfsstore.Blob{ID: uuid.NewString(), Meta: []byte(`{}`), Data: []byte("data")}
	require.NoError(t, s.CreateBlob(ctx, blob))

	file := &vfsstore.File{
		ID:      uuid.NewString(),
		Type:    "text/plain",
		Meta:    []byte(`{"description": "To be deleted"}`),
		BlobsID: blob.ID,
	}
	require.NoError(t, s.CreateFile(ctx, file))
	require.NoError(t, s.DeleteFile(ctx, file.ID))

	_, err := s.GetFileByID(ctx, file.ID)
	require.ErrorIs(t, err, libdb.ErrNotFound)
}

func TestUnit_GetFileByIDNotFound(t *testing.T) {
	ctx, s := SetupStore(t)

	_, err := s.GetFileByID(ctx, uuid.NewString())
	require.ErrorIs(t, err, libdb.ErrNotFound)
}

func TestUnit_ListAll(t *testing.T) {
	ctx, s := SetupStore(t)

	files, err := s.ListFiles(ctx)
	require.NoError(t, err)
	require.Len(t, files, 0)

	for i := 0; i < 3; i++ {
		// Files may have no blob (blobs_id is nullable) — omit BlobsID to avoid FK violation.
		require.NoError(t, s.CreateFile(ctx, &vfsstore.File{
			ID:   uuid.NewString(),
			Type: "text/plain",
			Meta: []byte(`{}`),
		}))
	}

	files, err = s.ListFiles(ctx)
	require.NoError(t, err)
	require.Len(t, files, 3)
}



func TestUnit_CreateAndGetFileNameID(t *testing.T) {
	ctx, s := SetupStore(t)

	id := newFileID(t, s, ctx)
	parentID := newFileID(t, s, ctx)
	name := "example.txt"

	require.NoError(t, s.CreateFileNameID(ctx, id, parentID, name))

	gotName, err := s.GetFileNameByID(ctx, id)
	require.NoError(t, err)
	require.Equal(t, name, gotName)

	gotParentID, err := s.GetFileParentID(ctx, id)
	require.NoError(t, err)
	require.Equal(t, parentID, gotParentID)
}

func TestUnit_UpdateFileNameByID(t *testing.T) {
	ctx, s := SetupStore(t)

	id := newFileID(t, s, ctx)
	parentID := newFileID(t, s, ctx)
	require.NoError(t, s.CreateFileNameID(ctx, id, parentID, "initial.txt"))

	require.NoError(t, s.UpdateFileNameByID(ctx, id, "updated.txt"))

	gotName, err := s.GetFileNameByID(ctx, id)
	require.NoError(t, err)
	require.Equal(t, "updated.txt", gotName)
}

func TestUnit_DeleteFileNameID(t *testing.T) {
	ctx, s := SetupStore(t)

	id := newFileID(t, s, ctx)
	parentID := newFileID(t, s, ctx)
	require.NoError(t, s.CreateFileNameID(ctx, id, parentID, "todelete.txt"))
	require.NoError(t, s.DeleteFileNameID(ctx, id))

	_, err := s.GetFileNameByID(ctx, id)
	require.ErrorIs(t, err, libdb.ErrNotFound)
}

func TestUnit_ListFileIDsByParentID(t *testing.T) {
	ctx, s := SetupStore(t)

	parentID := newFileID(t, s, ctx)
	id1 := newFileID(t, s, ctx)
	id2 := newFileID(t, s, ctx)

	require.NoError(t, s.CreateFileNameID(ctx, id1, parentID, "a.txt"))
	require.NoError(t, s.CreateFileNameID(ctx, id2, parentID, "b.txt"))

	ids, err := s.ListFileIDsByParentID(ctx, parentID)
	require.NoError(t, err)
	require.ElementsMatch(t, []string{id1, id2}, ids)
}

func TestUnit_ListFileIDsByEmptyParentID(t *testing.T) {
	ctx, s := SetupStore(t)

	id1 := newFileID(t, s, ctx)
	id2 := newFileID(t, s, ctx)

	require.NoError(t, s.CreateFileNameID(ctx, id1, "", "a.txt"))
	require.NoError(t, s.CreateFileNameID(ctx, id2, "", "b.txt"))

	ids, err := s.ListFileIDsByParentID(ctx, "")
	require.NoError(t, err)
	require.ElementsMatch(t, []string{id1, id2}, ids)
}

func TestUnit_ListFileIDsByName(t *testing.T) {
	ctx, s := SetupStore(t)

	parentID := newFileID(t, s, ctx)
	id := newFileID(t, s, ctx)
	require.NoError(t, s.CreateFileNameID(ctx, id, parentID, "unique.txt"))

	ids, err := s.ListFileIDsByName(ctx, parentID, "unique.txt")
	require.NoError(t, err)
	require.Contains(t, ids, id)
}

func TestUnit_Blob_CreatesAndFetchesByID(t *testing.T) {
	ctx, s := SetupStore(t)

	blob := &vfsstore.Blob{
		ID:   uuid.NewString(),
		Meta: []byte(`{"description": "Test blob"}`),
		Data: []byte("binary data"),
	}

	require.NoError(t, s.CreateBlob(ctx, blob))
	require.NotZero(t, blob.CreatedAt)
	require.NotZero(t, blob.UpdatedAt)

	retrieved, err := s.GetBlobByID(ctx, blob.ID)
	require.NoError(t, err)
	require.Equal(t, blob.ID, retrieved.ID)
	require.Equal(t, blob.Meta, retrieved.Meta)
	require.Equal(t, blob.Data, retrieved.Data)
	require.WithinDuration(t, blob.CreatedAt, retrieved.CreatedAt, time.Second)
}

func TestUnit_Blob_GetNonexistentReturnsNotFound(t *testing.T) {
	ctx, s := SetupStore(t)

	_, err := s.GetBlobByID(ctx, uuid.NewString())
	require.ErrorIs(t, err, libdb.ErrNotFound)
}

func TestUnit_Blob_DeletesSuccessfully(t *testing.T) {
	ctx, s := SetupStore(t)

	blob := &vfsstore.Blob{
		ID:   uuid.NewString(),
		Meta: []byte(`{}`),
		Data: []byte("data"),
	}
	require.NoError(t, s.CreateBlob(ctx, blob))
	require.NoError(t, s.DeleteBlob(ctx, blob.ID))

	_, err := s.GetBlobByID(ctx, blob.ID)
	require.ErrorIs(t, err, libdb.ErrNotFound)
}

func TestUnit_UpdateBlob(t *testing.T) {
	ctx, s := SetupStore(t)

	blob := &vfsstore.Blob{
		ID:   uuid.NewString(),
		Meta: []byte(`{"v":1}`),
		Data: []byte("original"),
	}
	require.NoError(t, s.CreateBlob(ctx, blob))

	require.NoError(t, s.UpdateBlob(ctx, blob.ID, []byte("updated"), []byte(`{"v": 2}`)))

	got, err := s.GetBlobByID(ctx, blob.ID)
	require.NoError(t, err)
	require.Equal(t, []byte("updated"), got.Data)
	require.Equal(t, []byte(`{"v": 2}`), got.Meta)
}

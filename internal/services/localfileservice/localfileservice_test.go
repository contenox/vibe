package localfileservice_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	libdb "github.com/contenox/contenox/libdbexec"
	"github.com/contenox/contenox/internal/services/localfileservice"
	"github.com/stretchr/testify/require"
)

func TestUnit_LocalFileService_CRUD(t *testing.T) {
	ctx := context.Background()
	svc, err := localfileservice.New(t.TempDir())
	require.NoError(t, err)

	_, err = svc.Mkdir(ctx, "docs")
	require.NoError(t, err)
	entry, err := svc.Write(ctx, "docs/readme.txt", []byte("hello"), true)
	require.NoError(t, err)
	require.Equal(t, "docs/readme.txt", entry.Path)

	data, meta, err := svc.Read(ctx, "docs/readme.txt")
	require.NoError(t, err)
	require.Equal(t, []byte("hello"), data)
	require.Equal(t, "readme.txt", meta.Name)

	moved, err := svc.Move(ctx, "docs/readme.txt", "README.txt")
	require.NoError(t, err)
	require.Equal(t, "README.txt", moved.Path)

	items, err := svc.List(ctx, ".")
	require.NoError(t, err)
	require.Len(t, items, 2)

	require.NoError(t, svc.Delete(ctx, "README.txt"))
	_, _, err = svc.Read(ctx, "README.txt")
	require.ErrorIs(t, err, libdb.ErrNotFound)
}

func TestUnit_LocalFileService_RejectsTraversalAndSymlinkEscape(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("secret"), 0644))
	require.NoError(t, os.Symlink(outside, filepath.Join(root, "out")))

	svc, err := localfileservice.New(root)
	require.NoError(t, err)

	_, _, err = svc.Read(context.Background(), "../outside")
	require.ErrorIs(t, err, localfileservice.ErrInvalidPath)

	_, _, err = svc.Read(context.Background(), "out/secret.txt")
	require.ErrorIs(t, err, localfileservice.ErrInvalidPath)

	_, err = svc.Write(context.Background(), "out/new.txt", []byte("nope"), true)
	require.ErrorIs(t, err, localfileservice.ErrInvalidPath)
}

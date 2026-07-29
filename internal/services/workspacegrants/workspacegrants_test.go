package workspacegrants_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	libdb "github.com/contenox/contenox/libdbexec"
	"github.com/contenox/contenox/internal/services/workspacegrants"
	"github.com/contenox/contenox/internal/store/runtimetypes"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func setupStore(t *testing.T) (context.Context, runtimetypes.Store) {
	t.Helper()
	ctx := context.Background()
	db, err := libdb.NewSQLiteDBManager(ctx, filepath.Join(t.TempDir(), "grants.db"), runtimetypes.SchemaSQLite)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	return ctx, runtimetypes.New(db.WithoutTransaction())
}

func TestUnit_Add_ValidDirectoryIsGranted(t *testing.T) {
	ctx, store := setupStore(t)
	dir := t.TempDir()

	roots, err := workspacegrants.Add(ctx, store, dir)
	require.NoError(t, err)
	require.Len(t, roots, 1)
	assert.Equal(t, filepath.Clean(dir), roots[0], "the grant is stored as the cleaned absolute path")

	assert.Equal(t, roots, workspacegrants.ReadGrants(ctx, store), "ReadGrants returns what Add persisted")
}

func TestUnit_Add_IsIdempotent(t *testing.T) {
	ctx, store := setupStore(t)
	dir := t.TempDir()

	_, err := workspacegrants.Add(ctx, store, dir)
	require.NoError(t, err)
	roots, err := workspacegrants.Add(ctx, store, dir)
	require.NoError(t, err)
	assert.Len(t, roots, 1, "granting the same directory twice does not duplicate it")
}

func TestUnit_Add_RejectsBadPaths(t *testing.T) {
	ctx, store := setupStore(t)

	t.Run("non-existent path", func(t *testing.T) {
		_, err := workspacegrants.Add(ctx, store, filepath.Join(t.TempDir(), "nope"))
		require.Error(t, err)
		require.ErrorIs(t, err, workspacegrants.ErrInvalidGrant)
		require.Contains(t, err.Error(), "does not exist")
	})

	t.Run("a file, not a directory", func(t *testing.T) {
		dir := t.TempDir()
		file := filepath.Join(dir, "f.txt")
		require.NoError(t, os.WriteFile(file, []byte("x"), 0o644))
		_, err := workspacegrants.Add(ctx, store, file)
		require.Error(t, err)
		require.ErrorIs(t, err, workspacegrants.ErrInvalidGrant)
		require.Contains(t, err.Error(), "not a directory")
	})

	t.Run("empty path", func(t *testing.T) {
		_, err := workspacegrants.Add(ctx, store, "   ")
		require.Error(t, err)
		require.ErrorIs(t, err, workspacegrants.ErrInvalidGrant)
	})

	// A rejected grant must not have written anything.
	assert.Empty(t, workspacegrants.ReadGrants(ctx, store))
}

func TestUnit_TwoGrantsRoundTripThroughStorage(t *testing.T) {
	ctx, store := setupStore(t)
	a := t.TempDir()
	b := t.TempDir()

	_, err := workspacegrants.Add(ctx, store, a)
	require.NoError(t, err)
	roots, err := workspacegrants.Add(ctx, store, b)
	require.NoError(t, err)

	require.Equal(t, []string{filepath.Clean(a), filepath.Clean(b)}, roots,
		"grants preserve insertion order and round-trip through the path-list storage format")
	require.Equal(t, roots, workspacegrants.ReadGrants(ctx, store))
}

func TestUnit_Remove(t *testing.T) {
	ctx, store := setupStore(t)
	a := t.TempDir()
	b := t.TempDir()
	_, err := workspacegrants.Add(ctx, store, a)
	require.NoError(t, err)
	_, err = workspacegrants.Add(ctx, store, b)
	require.NoError(t, err)

	roots, err := workspacegrants.Remove(ctx, store, a)
	require.NoError(t, err)
	require.Equal(t, []string{filepath.Clean(b)}, roots, "remove drops exactly the named grant")

	// Removing a path that was never granted is an idempotent no-op.
	roots, err = workspacegrants.Remove(ctx, store, t.TempDir())
	require.NoError(t, err)
	require.Equal(t, []string{filepath.Clean(b)}, roots)

	// A grant to a since-deleted directory can still be revoked (no existence check).
	gone := t.TempDir()
	_, err = workspacegrants.Add(ctx, store, gone)
	require.NoError(t, err)
	require.NoError(t, os.RemoveAll(gone))
	roots, err = workspacegrants.Remove(ctx, store, gone)
	require.NoError(t, err)
	require.Equal(t, []string{filepath.Clean(b)}, roots)
}

// fakePublisher captures the one doorbell publish so a test can assert the
// subject and decoded payload.
type fakePublisher struct {
	subject string
	data    []byte
	err     error
}

func (p *fakePublisher) Publish(_ context.Context, subject string, data []byte) error {
	p.subject = subject
	p.data = append([]byte(nil), data...)
	return p.err
}

func TestUnit_PublishChanged(t *testing.T) {
	ctx := context.Background()

	t.Run("publishes the roots on the reload subject", func(t *testing.T) {
		pub := &fakePublisher{}
		roots := []string{"/a", "/b"}
		require.NoError(t, workspacegrants.PublishChanged(ctx, pub, roots))
		require.Equal(t, workspacegrants.RootsChangedSubject, pub.subject)
		var ev workspacegrants.RootsChangedEvent
		require.NoError(t, json.Unmarshal(pub.data, &ev))
		require.Equal(t, roots, ev.Roots)
	})

	t.Run("a nil publisher is a no-op", func(t *testing.T) {
		require.NoError(t, workspacegrants.PublishChanged(ctx, nil, []string{"/a"}))
	})

	t.Run("a publish failure surfaces for the caller to log", func(t *testing.T) {
		pub := &fakePublisher{err: errors.New("bus down")}
		require.Error(t, workspacegrants.PublishChanged(ctx, pub, []string{"/a"}))
	})
}

func TestUnit_Add_RefusesBroadParents(t *testing.T) {
	ctx, store := setupStore(t)

	// The filesystem root and top-level system dirs (single-segment, e.g. /tmp)
	// are refused: granting them would defeat project scoping.
	for _, broad := range []string{string(filepath.Separator), filepath.Join(string(filepath.Separator), "tmp")} {
		_, err := workspacegrants.Add(ctx, store, broad)
		require.ErrorIs(t, err, workspacegrants.ErrInvalidGrant, "broad parent %q must be refused", broad)
	}

	// The operator's home directory is refused.
	home := t.TempDir()
	t.Setenv("HOME", home)
	_, err := workspacegrants.Add(ctx, store, home)
	require.ErrorIs(t, err, workspacegrants.ErrInvalidGrant, "the home directory must be refused")

	// A specific project directory (>=2 segments deep) is accepted.
	proj := filepath.Join(home, "proj")
	require.NoError(t, os.MkdirAll(proj, 0o750))
	got, err := workspacegrants.Add(ctx, store, proj)
	require.NoError(t, err)
	require.Contains(t, got, proj)
}

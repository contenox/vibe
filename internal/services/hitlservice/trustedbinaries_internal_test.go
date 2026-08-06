package hitlservice

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// writeAt writes content and stamps a fixed mtime, so the cache's change
// signal is controlled rather than observed.
func writeAt(t *testing.T, path, content string, mtime time.Time) {
	t.Helper()
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
	require.NoError(t, os.Chtimes(path, mtime, mtime))
}

// TestUnit_BinaryHashCache_RehashesOnChange pins the cache key: (path, size,
// mtime), portable and deliberately without inode, which Windows has none of.
func TestUnit_BinaryHashCache_RehashesOnChange(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tool")
	t0 := time.Now().Add(-time.Hour).Truncate(time.Second)

	writeAt(t, path, "aaaa", t0)
	first, err := binarySHA256(path)
	require.NoError(t, err)
	again, err := binarySHA256(path)
	require.NoError(t, err)
	assert.Equal(t, first, again, "an unchanged file must not be re-read")

	// Same size, later mtime: the change signal fires.
	writeAt(t, path, "bbbb", t0.Add(time.Minute))
	changed, err := binarySHA256(path)
	require.NoError(t, err)
	assert.NotEqual(t, first, changed, "a same-size rewrite with a new mtime must be re-hashed")
	assert.Equal(t, mustHash(t, path), changed)

	// Same mtime, different size: also a change signal.
	writeAt(t, path, "bbbbbbbb", t0.Add(time.Minute))
	resized, err := binarySHA256(path)
	require.NoError(t, err)
	assert.Equal(t, mustHash(t, path), resized, "a size change must be re-hashed even at an unchanged mtime")
}

// TestUnit_BinaryHashCache_TrustsSizeAndMtime pins the cache's stated trust
// assumption rather than pretending it away: someone who can rewrite the
// binary can also restore its size and mtime, and the cache will then serve
// the stale digest. That is inside the properly-set-up-system assumption —
// write access to a trusted directory is already past this control — and is
// documented in docs/guide/trusted-binaries.md.
func TestUnit_BinaryHashCache_TrustsSizeAndMtime(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tool")
	stamp := time.Now().Add(-time.Hour).Truncate(time.Second)

	writeAt(t, path, "aaaa", stamp)
	cached, err := binarySHA256(path)
	require.NoError(t, err)

	writeAt(t, path, "cccc", stamp) // same length, mtime restored
	stale, err := binarySHA256(path)
	require.NoError(t, err)
	assert.Equal(t, cached, stale, "the cache trusts (size, mtime); this limitation is documented, not defended against")
	assert.NotEqual(t, mustHash(t, path), stale, "and the bytes on disk really did change")
}

func mustHash(t *testing.T, path string) string {
	t.Helper()
	sum, err := hashFileSHA256(path)
	require.NoError(t, err)
	return sum
}

// TestUnit_ResolveBinary_RefusesWhatItCannotName pins that every failure to
// answer "which file will this run" is an error, never a lenient guess.
func TestUnit_ResolveBinary_RefusesWhatItCannotName(t *testing.T) {
	for _, name := range []string{"", "   ", "contenox-no-such-binary-xyz"} {
		_, err := resolveBinary(name)
		assert.Errorf(t, err, "%q must not resolve", name)
	}
	_, err := resolveBinary("./tool")
	assert.ErrorIs(t, err, errRelativeCommand)
}

// TestUnit_ResolveBinary_FollowsSymlinks pins that an alias never stands in
// for the file it points at — the check is on the real path.
func TestUnit_ResolveBinary_FollowsSymlinks(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "real")
	require.NoError(t, os.WriteFile(real, []byte("x"), 0o755))
	link := filepath.Join(dir, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlinks unavailable on this host: %v", err)
	}
	resolved, err := resolveBinary(link)
	require.NoError(t, err)
	realResolved, err := filepath.EvalSymlinks(real)
	require.NoError(t, err)
	assert.Equal(t, filepath.Clean(realResolved), resolved)
}

// TestUnit_UnderAnyTrustedDir_IsAPathPrefixNotAStringPrefix pins that a
// sibling directory sharing a name prefix is not "under" a trusted dir.
func TestUnit_UnderAnyTrustedDir_IsAPathPrefixNotAStringPrefix(t *testing.T) {
	root := t.TempDir()
	trusted := filepath.Join(root, "bin")
	sibling := filepath.Join(root, "bin-evil")
	require.NoError(t, os.MkdirAll(filepath.Join(trusted, "nested"), 0o755))
	require.NoError(t, os.MkdirAll(sibling, 0o755))

	assert.True(t, underAnyTrustedDir(filepath.Join(trusted, "tool"), []string{trusted}))
	assert.True(t, underAnyTrustedDir(filepath.Join(trusted, "nested", "tool"), []string{trusted}),
		"a trusted dir covers any depth under it")
	assert.False(t, underAnyTrustedDir(filepath.Join(sibling, "tool"), []string{trusted}),
		"bin-evil/ is not under bin/")
	assert.False(t, underAnyTrustedDir(filepath.Join(trusted, "tool"), nil))
}

package acpsvc

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	libacp "github.com/contenox/contenox/libacp"
	"github.com/stretchr/testify/require"
)

// fsListTestTransport builds a transport whose one session is rooted at a
// throwaway workspace, and returns the root so a case can plant files in it.
func fsListTestTransport(t *testing.T) (*Transport, libacp.SessionID, string) {
	t.Helper()
	root := t.TempDir()
	sid := libacp.SessionID("sess-acp")
	tr := &Transport{
		sessions: map[libacp.SessionID]*sessionEntry{sid: {Cwd: root, InternalSessionID: "sess-internal"}},
	}
	return tr, sid, root
}

func listDir(t *testing.T, tr *Transport, p fsListParams) (fsListResult, *libacp.Error) {
	t.Helper()
	raw, err := json.Marshal(p)
	require.NoError(t, err)
	out, rpcErr := tr.handleFSList(context.Background(), raw)
	if rpcErr != nil {
		return fsListResult{}, rpcErr
	}
	var res fsListResult
	require.NoError(t, json.Unmarshal(out, &res))
	return res, nil
}

func write(t *testing.T, root, rel string) {
	t.Helper()
	abs := filepath.Join(root, filepath.FromSlash(rel))
	require.NoError(t, os.MkdirAll(filepath.Dir(abs), 0o755))
	require.NoError(t, os.WriteFile(abs, []byte("x"), 0o644))
}

func TestUnit_FSList_ListsOneLevelDirectoriesFirst(t *testing.T) {
	tr, sid, root := fsListTestTransport(t)
	write(t, root, "zebra.txt")
	write(t, root, "Alpha.md")
	write(t, root, "src/main.go")
	require.NoError(t, os.MkdirAll(filepath.Join(root, "docs"), 0o755))

	res, rpcErr := listDir(t, tr, fsListParams{SessionID: string(sid)})
	require.Nil(t, rpcErr)
	require.Equal(t, ".", res.Path)

	names := make([]string, 0, len(res.Entries))
	for _, e := range res.Entries {
		names = append(names, e.Name)
	}
	require.Equal(t, []string{"docs", "src", "Alpha.md", "zebra.txt"}, names,
		"directories first, then case-insensitive by name — the order a person scanning a tree expects")

	// One LEVEL: src/main.go is not in the root's answer.
	require.NotContains(t, names, "main.go")
}

func TestUnit_FSList_DescendsByRootRelativePath(t *testing.T) {
	tr, sid, root := fsListTestTransport(t)
	write(t, root, "src/inner/deep.go")

	res, rpcErr := listDir(t, tr, fsListParams{SessionID: string(sid), Path: "src"})
	require.Nil(t, rpcErr)
	require.Equal(t, "src", res.Path)
	require.Len(t, res.Entries, 1)
	require.Equal(t, "inner", res.Entries[0].Name)
	// The path is what the client hands straight back as the next Path, so it
	// is root-relative and slash-separated on every platform.
	require.Equal(t, "src/inner", res.Entries[0].Path)
	require.True(t, res.Entries[0].IsDir)
}

// The picker must show the same workspace the agent works in: `local_fs`'s own
// listing hides these, so offering them here would be showing a different tree.
func TestUnit_FSList_AppliesTheSameNoiseFilterAsTheAgent(t *testing.T) {
	tr, sid, root := fsListTestTransport(t)
	write(t, root, "keep.go")
	write(t, root, "node_modules/pkg/index.js")
	write(t, root, ".git/config")

	res, rpcErr := listDir(t, tr, fsListParams{SessionID: string(sid)})
	require.Nil(t, rpcErr)
	names := map[string]bool{}
	for _, e := range res.Entries {
		names[e.Name] = true
	}
	require.True(t, names["keep.go"])
	require.False(t, names["node_modules"], "default skip-directories are not offered")
	require.False(t, names[".git"])
}

func TestUnit_FSList_HonoursTheWorkspaceGitignore(t *testing.T) {
	tr, sid, root := fsListTestTransport(t)
	require.NoError(t, os.WriteFile(filepath.Join(root, ".gitignore"), []byte("secret.env\n"), 0o644))
	write(t, root, "secret.env")
	write(t, root, "public.txt")

	res, rpcErr := listDir(t, tr, fsListParams{SessionID: string(sid)})
	require.Nil(t, rpcErr)
	for _, e := range res.Entries {
		require.NotEqual(t, "secret.env", e.Name, "the workspace .gitignore is the same predicate list_dir applies")
	}
}

// Refused rather than clamped: a caller that asked for ".." meant something,
// and answering with the root instead lets a browsing UI believe it escaped.
func TestUnit_FSList_RefusesEscapesAndAbsolutePaths(t *testing.T) {
	tr, sid, root := fsListTestTransport(t)
	write(t, root, "inside.txt")

	for _, bad := range []string{"..", "../", "../..", "src/../../..", "/etc", "/"} {
		_, rpcErr := listDir(t, tr, fsListParams{SessionID: string(sid), Path: bad})
		require.NotNil(t, rpcErr, "path %q must be refused", bad)
		require.Equal(t, libacp.ErrInvalidParams, rpcErr.Code, "path %q", bad)
	}

	// A traversal that lands back inside is still fine — the rule is about
	// leaving the root, not about the characters used to say so.
	write(t, root, "a/b.txt")
	res, rpcErr := listDir(t, tr, fsListParams{SessionID: string(sid), Path: "a/../a"})
	require.Nil(t, rpcErr)
	require.Equal(t, "a", res.Path)
}

// A symlink inside the workspace pointing out of it is an escape the string
// rules above cannot see; vfs.Contain resolves before comparing.
func TestUnit_FSList_RefusesASymlinkOutOfTheWorkspace(t *testing.T) {
	tr, sid, root := fsListTestTransport(t)
	outside := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("x"), 0o644))
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	_, rpcErr := listDir(t, tr, fsListParams{SessionID: string(sid), Path: "escape"})
	require.NotNil(t, rpcErr)
	require.Equal(t, libacp.ErrInvalidParams, rpcErr.Code)
}

func TestUnit_FSList_RefusesAnUnknownSessionAndAMissingDirectory(t *testing.T) {
	tr, sid, _ := fsListTestTransport(t)

	_, rpcErr := listDir(t, tr, fsListParams{SessionID: "nope"})
	require.NotNil(t, rpcErr)
	require.Equal(t, libacp.ErrInvalidParams, rpcErr.Code)

	_, rpcErr = listDir(t, tr, fsListParams{SessionID: ""})
	require.NotNil(t, rpcErr)

	_, rpcErr = listDir(t, tr, fsListParams{SessionID: string(sid), Path: "does/not/exist"})
	require.NotNil(t, rpcErr)
	require.Equal(t, libacp.ErrInvalidParams, rpcErr.Code)
}

// A session with no workspace has nothing to list, and says so as a mode rather
// than answering an empty directory that looks like an empty workspace.
func TestUnit_FSList_RefusesASessionWithNoWorkspace(t *testing.T) {
	sid := libacp.SessionID("sess-acp")
	tr := &Transport{sessions: map[libacp.SessionID]*sessionEntry{sid: {}}}

	_, rpcErr := listDir(t, tr, fsListParams{SessionID: string(sid)})
	require.NotNil(t, rpcErr)
	require.Equal(t, libacp.ErrMethodNotFound, rpcErr.Code)
}

func TestUnit_FSList_CapsAHugeDirectoryAndSaysSo(t *testing.T) {
	tr, sid, root := fsListTestTransport(t)
	for i := 0; i < fsListMaxEntries+25; i++ {
		write(t, root, filepath.ToSlash(filepath.Join("big", "f"+itoa(i)+".txt")))
	}

	res, rpcErr := listDir(t, tr, fsListParams{SessionID: string(sid), Path: "big"})
	require.Nil(t, rpcErr)
	require.Len(t, res.Entries, fsListMaxEntries)
	require.True(t, res.Truncated, "a capped listing must not imply it was complete")
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}

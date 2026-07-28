package workspaceindex

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/contenox/contenox/internal/models/ollamatokenizer"
	"github.com/stretchr/testify/require"
)

// fixedTokenizer charges one token per line, so chunk boundaries in these
// tests are exact line counts rather than estimates.
type fixedTokenizer struct{ perLine int }

func (f fixedTokenizer) CountTokens(ctx context.Context, model, prompt string) (int, error) {
	return f.perLine, nil
}

func numberedFile(t *testing.T, lines int) sourceFile {
	t.Helper()
	body := make([]string, lines)
	for i := range body {
		body[i] = "line " + strconv.Itoa(i+1)
	}
	content := strings.Join(body, "\n") + "\n"
	return sourceFile{RelPath: "a.txt", Content: content, SHA: fileSHA([]byte(content))}
}

func TestUnit_ChunkFile_LineRangesAreExactAndOverlap(t *testing.T) {
	ctx := context.Background()
	f := numberedFile(t, 20)

	// budget 5 tokens at 1 token/line => 5 lines per chunk, overlapping by 2.
	chunks, err := chunkFile(ctx, fixedTokenizer{perLine: 1}, "m", f, 5, 2)
	require.NoError(t, err)
	require.NotEmpty(t, chunks)

	require.Equal(t, 1, chunks[0].StartLine)
	require.Equal(t, 5, chunks[0].EndLine)
	require.Equal(t, "line 1\nline 2\nline 3\nline 4\nline 5", chunks[0].Text)

	// Each subsequent chunk repeats exactly `overlap` lines of its predecessor.
	for i := 1; i < len(chunks); i++ {
		prev, cur := chunks[i-1], chunks[i]
		require.Equal(t, prev.EndLine-2+1, cur.StartLine, "chunk %d must start 2 lines back into chunk %d", i, i-1)
		require.Greater(t, cur.StartLine, prev.StartLine, "chunking must always make progress")
	}

	// Every line must appear in some chunk, and each chunk's text must equal its named range.
	all := splitLines(f.Content)
	covered := map[int]bool{}
	for _, c := range chunks {
		require.Equal(t, strings.Join(all[c.StartLine-1:c.EndLine], "\n"), c.Text,
			"chunk %s:%d-%d text must equal those exact lines", c.Path, c.StartLine, c.EndLine)
		for ln := c.StartLine; ln <= c.EndLine; ln++ {
			covered[ln] = true
		}
	}
	require.Len(t, covered, len(all), "every line must be indexed at least once")
	require.Equal(t, len(all), chunks[len(chunks)-1].EndLine, "the last chunk must reach the last line")
}

func TestUnit_ChunkFile_ShaIsStableAndPerFile(t *testing.T) {
	ctx := context.Background()
	f := numberedFile(t, 12)

	first, err := chunkFile(ctx, fixedTokenizer{perLine: 1}, "m", f, 4, 1)
	require.NoError(t, err)
	second, err := chunkFile(ctx, fixedTokenizer{perLine: 1}, "m", f, 4, 1)
	require.NoError(t, err)
	require.Equal(t, first, second, "chunking must be deterministic")

	for _, c := range first {
		require.Equal(t, f.SHA, c.SHA, "every chunk carries the WHOLE FILE's sha, not its own")
	}

	// A one-character edit changes the sha for every chunk.
	edited := f
	edited.Content = strings.Replace(f.Content, "line 7", "line seven", 1)
	edited.SHA = fileSHA([]byte(edited.Content))
	require.NotEqual(t, f.SHA, edited.SHA)
}

// TestUnit_ChunkFile_OversizedLineSurvivesWhole pins that a line over budget is still indexed, never split mid-line or dropped.
func TestUnit_ChunkFile_OversizedLineSurvivesWhole(t *testing.T) {
	ctx := context.Background()
	long := strings.Repeat("x", 4000)
	content := "short\n" + long + "\nshort again\n"
	f := sourceFile{RelPath: "a.txt", Content: content, SHA: fileSHA([]byte(content))}

	chunks, err := chunkFile(ctx, ollamatokenizer.NewEstimateTokenizer(), "m", f, 50, 1)
	require.NoError(t, err)

	var found bool
	for _, c := range chunks {
		if strings.Contains(c.Text, long) {
			found = true
		}
	}
	require.True(t, found, "an over-budget line must be kept whole, not discarded")
}

func TestUnit_ChunkFile_EmptyAndBlankFiles(t *testing.T) {
	ctx := context.Background()
	tok := fixedTokenizer{perLine: 1}

	chunks, err := chunkFile(ctx, tok, "m", sourceFile{RelPath: "empty.txt"}, 10, 2)
	require.NoError(t, err)
	require.Empty(t, chunks)

	blank := sourceFile{RelPath: "blank.txt", Content: "\n\n   \n"}
	chunks, err = chunkFile(ctx, tok, "m", blank, 10, 2)
	require.NoError(t, err)
	require.Empty(t, chunks, "whitespace-only content produces nothing worth embedding")
}

// TestUnit_ChunkFile_OverlapLargerThanChunkTerminates pins that overlap larger than the chunk size still terminates.
func TestUnit_ChunkFile_OverlapLargerThanChunkTerminates(t *testing.T) {
	ctx := context.Background()
	f := numberedFile(t, 10)
	chunks, err := chunkFile(ctx, fixedTokenizer{perLine: 1}, "m", f, 2, 50)
	require.NoError(t, err)
	require.NotEmpty(t, chunks)
	require.Equal(t, 10, chunks[len(chunks)-1].EndLine)
}

func TestUnit_ChunkFile_UsesTheRealEstimator(t *testing.T) {
	ctx := context.Background()
	f := numberedFile(t, 200)
	chunks, err := chunkFile(ctx, ollamatokenizer.NewEstimateTokenizer(), "m", f, chunkTokensDefault, overlapLinesDefault)
	require.NoError(t, err)
	require.NotEmpty(t, chunks)
	for _, c := range chunks {
		require.LessOrEqual(t, c.EndLine-c.StartLine+1, 400, "a chunk must stay within the token budget's reach")
	}
}

func TestUnit_ChunkTokensForContext(t *testing.T) {
	require.Equal(t, chunkTokensDefault, ChunkTokensForContext(0), "an unknown limit falls back to the default")
	require.Equal(t, 384, ChunkTokensForContext(512), "a quarter of the window is headroom for the estimator's error")
	require.Equal(t, 64, ChunkTokensForContext(8), "a tiny limit still yields a usable floor")
}

// --- File selection ---

func writeFile(t *testing.T, root, rel, content string) {
	t.Helper()
	abs := filepath.Join(root, filepath.FromSlash(rel))
	require.NoError(t, os.MkdirAll(filepath.Dir(abs), 0o755))
	require.NoError(t, os.WriteFile(abs, []byte(content), 0o644))
}

func collectWalk(t *testing.T, root string, maxBytes int64) ([]string, walkStats) {
	t.Helper()
	var paths []string
	stats, err := walkWorkspace(context.Background(), root, maxBytes, func(f sourceFile) error {
		paths = append(paths, f.RelPath)
		return nil
	})
	require.NoError(t, err)
	return paths, stats
}

// TestUnit_WalkWorkspace_HonoursTheNoiseFilter pins that selection uses the same noise filter as find_files and @-completion.
func TestUnit_WalkWorkspace_HonoursTheNoiseFilter(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "keep.md", "kept content\n")
	writeFile(t, root, "src/keep.go", "package src\n")
	writeFile(t, root, ".gitignore", "ignored.txt\nbuild-output/\n")
	writeFile(t, root, "ignored.txt", "gitignored\n")
	writeFile(t, root, "build-output/artifact.txt", "gitignored dir\n")
	writeFile(t, root, "node_modules/pkg/index.js", "skip-dir basename\n")
	writeFile(t, root, ".git/config", "skip-dir basename\n")
	writeFile(t, root, "vendor/dep/dep.go", "skip-dir basename\n")

	paths, stats := collectWalk(t, root, maxFileBytesDefault)
	require.ElementsMatch(t, []string{".gitignore", "keep.md", "src/keep.go"}, paths)
	require.Equal(t, 3, stats.Selected)
}

func TestUnit_WalkWorkspace_SkipsBinaryAndOversize(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "text.md", "readable prose\n")
	writeFile(t, root, "blob.bin", "PNG\x00\x00\x00garbage\x00\x00")
	writeFile(t, root, "huge.txt", strings.Repeat("a", 4096))

	paths, stats := collectWalk(t, root, 1024)
	require.Equal(t, []string{"text.md"}, paths)
	require.Equal(t, 1, stats.SkippedBinary, "a NUL-bearing file must be sniffed as binary, not indexed")
	require.Equal(t, 1, stats.SkippedOversize, "a file over the cap is counted, never silently forgotten")
	require.EqualValues(t, len("readable prose\n"), stats.Bytes)
}

// TestUnit_WalkWorkspace_SkipsDependencyLockfiles pins that a lockfile under the size cap is refused by name and the refusal is counted.
func TestUnit_WalkWorkspace_SkipsDependencyLockfiles(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "README.md", "prose a human wrote\n")
	writeFile(t, root, "go.mod", "module example.com/x\n")
	writeFile(t, root, "go.sum", "example.com/dep v1.0.0 h1:abc=\nexample.com/dep v1.0.0/go.mod h1:def=\n")
	writeFile(t, root, "website/package-lock.json", `{"name":"w","lockfileVersion":3,"packages":{}}`)
	writeFile(t, root, "Cargo.lock", "[[package]]\nname = \"x\"\n")

	paths, stats := collectWalk(t, root, maxFileBytesDefault)
	require.ElementsMatch(t, []string{"README.md", "go.mod"}, paths,
		"a dependency lockfile was indexed; go.mod is NOT one — it is hand-edited and answers real questions")
	require.Equal(t, 3, stats.SkippedGenerated, "the refused lockfiles were not counted")
	require.Equal(t, 2, stats.Selected)
	require.Zero(t, stats.SkippedBinary)
	require.Zero(t, stats.SkippedOversize)
}

// TestUnit_WalkWorkspace_RefusesEscapingSymlink pins that a symlink pointing outside the tree is never read.
func TestUnit_WalkWorkspace_RefusesEscapingSymlink(t *testing.T) {
	outside := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("not yours\n"), 0o644))

	root := t.TempDir()
	writeFile(t, root, "ok.md", "fine\n")
	if err := os.Symlink(filepath.Join(outside, "secret.txt"), filepath.Join(root, "escape.txt")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	paths, _ := collectWalk(t, root, maxFileBytesDefault)
	require.Equal(t, []string{"ok.md"}, paths)
}

func TestUnit_WalkWorkspace_PropagatesCancellation(t *testing.T) {
	root := t.TempDir()
	for i := range 20 {
		writeFile(t, root, fmt.Sprintf("f%02d.md", i), "content\n")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := walkWorkspace(ctx, root, maxFileBytesDefault, func(sourceFile) error { return nil })
	require.ErrorIs(t, err, context.Canceled)
}

func TestUnit_SplitLines_NoPhantomTrailingLine(t *testing.T) {
	require.Equal(t, []string{"a", "b"}, splitLines("a\nb\n"))
	require.Equal(t, []string{"a", "b"}, splitLines("a\nb"))
	require.Equal(t, []string{"a", "b"}, splitLines("a\r\nb\r\n"))
	require.Nil(t, splitLines(""))
}

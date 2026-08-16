package localtools_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/contenox/contenox/internal/services/localtools"
	"github.com/stretchr/testify/require"
)

func writeExecutableBinary(t *testing.T, dir, name string, size int) string {
	t.Helper()
	p := filepath.Join(dir, name)
	content := make([]byte, size)
	for i := range content {
		content[i] = byte(i % 256)
	}
	require.NoError(t, os.WriteFile(p, content, 0755))
	return p
}

// TestUnit_ListDir_AnnotatesExecutableAndLargeFiles asserts list_dir annotates entries with '/' for directories, '*' for the executable bit, and a compact size once a file is large enough to matter.
// TestUnit_ListDir_SmallExecutableGetsNoSizeAnnotation asserts a small executable gets only the '*' marker, never a size annotation.
// TestUnit_ListDir_RecursiveAnnotatesExecutableAndLargeFiles asserts the recursive listing carries the same annotations as the non-recursive one.
// TestUnit_ListDir_NonDirectoryErrorDescribesWhatItIs asserts the non-directory error names what the path actually is (regular file, size, executable, binary).
// TestUnit_StatFile_ReportsExecutableAndBinaryFlags asserts stat_file's JSON reports executable/binary flags, and that ordinary files/directories are not falsely flagged.
// TestUnit_StatFile_SniffIsCheapOnLargeFiles asserts stat_file classifies a file far larger than read_file's size policy without loading the whole thing.
// TestUnit_ReadFile_RefusesBinaryWithTeachingError asserts read_file refuses binary content with a teaching error naming stat_file, rather than dumping raw bytes.
func TestUnit_ReadFile_RefusesBinaryWithTeachingError(t *testing.T) {
	dir := t.TempDir()
	// Small enough to pass the size gate untouched, exercising the content-based refusal rather than the size guard.
	writeExecutableBinary(t, dir, "contenox", 4096)

	h := localtools.NewLocalFSToolsForTest(dir, nil)
	_, err := execTool(t, context.Background(), h, "read_file", map[string]any{"path": "contenox"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "refusing to read")
	require.Contains(t, err.Error(), "binary")
	require.Contains(t, err.Error(), "executable")
	require.Contains(t, err.Error(), "shell tools")
}

// TestUnit_ReadFile_ExecutableTextFileIsStillReadable asserts an executable that is plain text is still readable: the refusal is about content, not the executable bit.
func TestUnit_ReadFile_ExecutableTextFileIsStillReadable(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "run.sh")
	require.NoError(t, os.WriteFile(p, []byte("#!/bin/sh\necho hi\n"), 0755))

	h := localtools.NewLocalFSToolsForTest(dir, nil)
	res, err := execTool(t, context.Background(), h, "read_file", map[string]any{"path": "run.sh"})
	require.NoError(t, err)
	require.Contains(t, res.(string), "echo hi")
}

// TestUnit_ReadFile_OversizedBinaryStillBlockedBySizeGuard asserts an oversized binary is still blocked by the pre-existing size gate, before the binary-content check ever runs.
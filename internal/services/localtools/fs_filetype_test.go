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

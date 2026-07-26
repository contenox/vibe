//go:build linux

package libsandbox

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// The load-bearing coupling invariant: every SystemExecDir — the dirs the emulated
// PATH is built from — must lie within the read+execute system runtime the Landlock
// wall actually grants (systemRuntimePaths). If it did not, the confined PATH would
// name a directory the wall denies, reintroducing exactly the findable-but-denied
// mismatch the single-source-of-truth design removes. This test is what keeps the
// two lists from drifting apart when someone edits either one.
func TestUnit_SystemExecDirs_WithinLandlockExecSurface(t *testing.T) {
	for _, execDir := range SystemExecDirs() {
		require.Truef(t, coveredBySystemRuntime(execDir),
			"SystemExecDir %q is not within any systemRuntimePaths entry — the emulated PATH would name a dir the Landlock wall does not grant", execDir)
	}
}

func coveredBySystemRuntime(execDir string) bool {
	p := filepath.Clean(execDir)
	for _, root := range systemRuntimePaths {
		root = filepath.Clean(root)
		if p == root || strings.HasPrefix(p, root+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

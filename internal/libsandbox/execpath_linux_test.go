//go:build linux

package libsandbox

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// Every SystemExecDir must lie within systemRuntimePaths (the Landlock exec grant), or the emulated PATH would name a dir the wall denies.
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

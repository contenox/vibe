package contenoxcli

import (
	"os"
	"path/filepath"

	"github.com/spf13/cobra"
)

type closerFunc func() error

func (f closerFunc) Close() error { return f() }

func contenoxDirForWorkspace(cmd *cobra.Command, dir string) (string, error) {
	if cmd != nil {
		if dataDir, _ := cmd.Root().PersistentFlags().GetString("data-dir"); dataDir != "" {
			return filepath.Abs(dataDir)
		}
	}
	cur, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	start := cur
	for {
		candidate := filepath.Join(cur, ".contenox")
		if info, statErr := os.Stat(candidate); statErr == nil && info.IsDir() {
			// A .contenox/ without workspace.id isn't a workspace; keep walking.
			if _, werr := os.Stat(filepath.Join(candidate, "workspace.id")); werr == nil {
				return candidate, nil
			}
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return filepath.Join(start, ".contenox"), nil
		}
		cur = parent
	}
}

package localtools_test

// Rec 4 (tool-hardening.md) for shell output, driven through LocalExecTools.Exec:
// when a command's output exceeds the context budget, the FULL stream is spooled
// to a durable file and the inline result carries a 20%-head/80%-tail split that
// names the spool concretely — nothing is dropped silently.

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/contenox/beam/internal/kernel/taskengine"
	"github.com/contenox/beam/internal/services/localtools"
	"github.com/stretchr/testify/require"
)

// linesRunner writes `n` numbered lines ("LINE0000\n"...) to stdout, one Write
// per line, so the spool writer's head/tail ring is exercised across many writes.
type linesRunner struct{ n int }

func (r linesRunner) Run(_ context.Context, _ localtools.CommandSpec, stdout, _ io.Writer) (int, error) {
	for i := 0; i < r.n; i++ {
		_, _ = io.WriteString(stdout, fmt.Sprintf("LINE%04d\n", i))
	}
	return 0, nil
}

func TestUnit_LocalShell_SpoolsFullOutputAndNamesIt(t *testing.T) {
	spoolDir := t.TempDir()
	t.Setenv("CONTENOX_TOOL_OUTPUT_DIR", spoolDir)

	h := localtools.NewLocalExecToolsWith(linesRunner{n: 200}).(*localtools.LocalExecTools)
	// Small budget forces truncation; total output is ~1800 bytes.
	ctx := context.WithValue(context.Background(), taskengine.ContextKeyOutputByteLimit, int64(200))
	call := &taskengine.ToolsCall{Name: "local_shell", Args: map[string]string{"command": "emit"}}

	out, dt, err := h.Exec(ctx, time.Now().UTC(), nil, false, call)
	require.NoError(t, err)
	require.Equal(t, taskengine.DataTypeJSON, dt)
	res := out.(*localtools.LocalExecResult)

	require.False(t, res.Success)
	require.Equal(t, -1, res.ExitCode)
	require.Contains(t, res.Error, "Output truncated")
	require.Contains(t, res.Error, "context budget")
	require.Contains(t, res.Error, "full output:")
	require.Contains(t, res.Error, "(recoverable:")

	// The inline result is a 20%-head/80%-tail split: it contains BOTH an early
	// line and a late line (the tail, where errors cluster), with an omission
	// marker between them.
	require.Contains(t, res.Stdout, "LINE0000", "head must retain the start of the stream")
	require.Contains(t, res.Stdout, "LINE0199", "tail must retain the end of the stream")
	require.Contains(t, res.Stdout, "omitted")
	require.NotContains(t, res.Stdout, "LINE0100", "the middle must be elided from the inline view")

	// The spool file holds the FULL stream, middle included.
	spoolPath := findOneSpoolFile(t, spoolDir)
	full, err := os.ReadFile(spoolPath)
	require.NoError(t, err)
	require.Contains(t, string(full), "LINE0000")
	require.Contains(t, string(full), "LINE0100", "the spool must contain the elided middle")
	require.Contains(t, string(full), "LINE0199")
	require.Contains(t, res.Error, filepath.Base(spoolPath), "the notice must name the actual spool file")
}

// A command whose output fits the budget must return in full and must NOT create
// a spool file — spooling is lazy, only on overflow.
func TestUnit_LocalShell_NoSpoolWhenWithinBudget(t *testing.T) {
	spoolDir := t.TempDir()
	t.Setenv("CONTENOX_TOOL_OUTPUT_DIR", spoolDir)

	h := localtools.NewLocalExecToolsWith(linesRunner{n: 3}).(*localtools.LocalExecTools)
	ctx := context.WithValue(context.Background(), taskengine.ContextKeyOutputByteLimit, int64(1<<20))
	call := &taskengine.ToolsCall{Name: "local_shell", Args: map[string]string{"command": "emit"}}

	out, _, err := h.Exec(ctx, time.Now().UTC(), nil, false, call)
	require.NoError(t, err)
	res := out.(*localtools.LocalExecResult)

	require.True(t, res.Success)
	require.Equal(t, "LINE0000\nLINE0001\nLINE0002", res.Stdout)
	require.Equal(t, 0, countSpoolFiles(spoolDir), "a within-budget command must not spool")
}

func findOneSpoolFile(t *testing.T, root string) string {
	t.Helper()
	var found string
	_ = filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() && found == "" {
			found = p
		}
		return nil
	})
	require.NotEmpty(t, found, "expected a spool file under %s", root)
	return found
}

func countSpoolFiles(root string) int {
	n := 0
	_ = filepath.WalkDir(root, func(_ string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			n++
		}
		return nil
	})
	return n
}

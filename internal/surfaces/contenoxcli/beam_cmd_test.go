package contenoxcli

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestUnit_RedirectBeamLogsToFile is the M7 contract in one place: after the
// redirect, a WARN goes to the file and NOT to stderr — beam is about to take
// that terminal, and a raw wrapped log line printed into the transcript's own
// scrollback is the defect this exists to remove.
func TestUnit_RedirectBeamLogsToFile(t *testing.T) {
	restore := slog.Default()
	t.Cleanup(func() { slog.SetDefault(restore) })

	dir := t.TempDir()
	dbPath := filepath.Join(dir, "scratch.db")

	logPath, closeLog, err := redirectBeamLogsToFile(dbPath)
	require.NoError(t, err)
	t.Cleanup(closeLog)

	require.Equal(t, filepath.Join(dir, beamLogFileName), logPath,
		"the log lives beside the database beam actually opened, so a --db scratch run keeps its warnings to itself")

	slog.Warn("beam-test-warning", "detail", "visible")
	slog.Info("beam-test-info")
	closeLog()

	raw, err := os.ReadFile(logPath)
	require.NoError(t, err)
	body := string(raw)
	require.Contains(t, body, "beam-test-warning", "WARN must reach the file")
	require.Contains(t, body, "visible")
	require.NotContains(t, body, "beam-test-info", "the level is WARN+: this is a log to open when something looked wrong, not a trace")
}

// TestUnit_RedirectBeamLogsToFile_AppendsAcrossRuns: the file accumulates, so an
// operator who quits and relaunches to reproduce something still has the first
// run's warnings when they finally open it.
func TestUnit_RedirectBeamLogsToFile_AppendsAcrossRuns(t *testing.T) {
	restore := slog.Default()
	t.Cleanup(func() { slog.SetDefault(restore) })

	dbPath := filepath.Join(t.TempDir(), "scratch.db")
	for _, msg := range []string{"first-run", "second-run"} {
		logPath, closeLog, err := redirectBeamLogsToFile(dbPath)
		require.NoError(t, err)
		slog.Warn(msg)
		closeLog()
		require.FileExists(t, logPath)
	}

	raw, err := os.ReadFile(filepath.Join(filepath.Dir(dbPath), beamLogFileName))
	require.NoError(t, err)
	require.Contains(t, string(raw), "first-run")
	require.Contains(t, string(raw), "second-run")
}

// TestUnit_RedirectBeamLogsToFile_ReportsAnUnwritableTarget: beam degrades to a
// warning on stderr rather than refusing to start, so this must return the
// error instead of installing a handler over a file it could not open.
func TestUnit_RedirectBeamLogsToFile_ReportsAnUnwritableTarget(t *testing.T) {
	restore := slog.Default()
	t.Cleanup(func() { slog.SetDefault(restore) })

	dbPath := filepath.Join(t.TempDir(), "no-such-dir", "scratch.db")
	logPath, closeLog, err := redirectBeamLogsToFile(dbPath)
	require.Error(t, err)
	require.Empty(t, logPath)
	require.Nil(t, closeLog)
	require.True(t, strings.Contains(err.Error(), beamLogFileName), "the error must name the path it failed on: %v", err)
	require.Same(t, restore, slog.Default(), "a failed redirect must leave logging exactly where it was")
}

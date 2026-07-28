package contenoxcli

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestUnit_RedirectBeamLogsToFile asserts a WARN goes to the file and never to stderr, which beam is about to take over as the screen.
func TestUnit_RedirectBeamLogsToFile(t *testing.T) {
	restore := slog.Default()
	t.Cleanup(func() { slog.SetDefault(restore) })

	dir := t.TempDir()
	dbPath := filepath.Join(dir, "scratch.db")

	logPath, _, closeLog, err := redirectBeamLogsToFile(dbPath)
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

// TestUnit_RedirectBeamLogsToFile_AppendsAcrossRuns asserts the log file accumulates warnings across relaunches rather than truncating.
func TestUnit_RedirectBeamLogsToFile_AppendsAcrossRuns(t *testing.T) {
	restore := slog.Default()
	t.Cleanup(func() { slog.SetDefault(restore) })

	dbPath := filepath.Join(t.TempDir(), "scratch.db")
	for _, msg := range []string{"first-run", "second-run"} {
		logPath, _, closeLog, err := redirectBeamLogsToFile(dbPath)
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

// TestUnit_BeamCmd_DefaultsToItsOwnPolicyAndChain pins beam's defaults: its
// own HITL preset (an attended-session envelope, not the shared acp/default
// ones) and its own chain (not `contenox acp`'s), each independently
// overridable via its own env var — editors keep using acp_cmd's defaults.
func TestUnit_BeamCmd_DefaultsToItsOwnPolicyAndChain(t *testing.T) {
	require.Equal(t, "hitl-policy-beam.json", beamHITLPolicy)
	require.Equal(t, "default-beam-chain.json", beamChainFile)
	require.Equal(t, "CONTENOX_BEAM_CHAIN_PATH", beamChainEnv)
	require.NotEqual(t, beamChainEnv, "CONTENOX_ACP_CHAIN_PATH", "beam must not share acp_cmd's chain override var")
}

// TestUnit_RedirectBeamLogsToFile_ReportsAnUnwritableTarget asserts an unwritable log target returns an error rather than installing a broken handler.
func TestUnit_RedirectBeamLogsToFile_ReportsAnUnwritableTarget(t *testing.T) {
	restore := slog.Default()
	t.Cleanup(func() { slog.SetDefault(restore) })

	dbPath := filepath.Join(t.TempDir(), "no-such-dir", "scratch.db")
	logPath, _, closeLog, err := redirectBeamLogsToFile(dbPath)
	require.Error(t, err)
	require.Empty(t, logPath)
	require.Nil(t, closeLog)
	require.True(t, strings.Contains(err.Error(), beamLogFileName), "the error must name the path it failed on: %v", err)
	require.Same(t, restore, slog.Default(), "a failed redirect must leave logging exactly where it was")
}

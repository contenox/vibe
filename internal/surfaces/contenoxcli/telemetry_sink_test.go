package contenoxcli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/contenox/contenox/internal/services/clikv"
	"github.com/contenox/contenox/internal/store/runtimetypes"
	"github.com/contenox/contenox/libtracker"
	"github.com/stretchr/testify/require"
)

// restoreDefaultSink puts the process default handler back on stderr, since
// setupTelemetryLogging installs it globally for every later test in the package.
func restoreDefaultSink(t *testing.T, ctx context.Context, store runtimetypes.Store, dir string) {
	t.Helper()
	t.Cleanup(func() {
		_ = clikv.SetString(ctx, store, "telemetry-enabled", "false")
		closeLogs, err := setupTelemetryLogging(ctx, store, dir, os.Stderr)
		if err == nil {
			closeLogs()
		}
	})
}

func probeDefaultSink(ctx context.Context, subject string) {
	_, _, end := libtracker.NewLogActivityTracker(nil).Start(ctx, "probe", subject)
	end()
}

func TestUnit_SetupTelemetryLogging_TeesDestAndTelemetryFile(t *testing.T) {
	ctx, _, store := setupSQLiteStore(t)
	dir := t.TempDir()
	restoreDefaultSink(t, ctx, store, dir)
	require.NoError(t, clikv.SetString(ctx, store, "telemetry-enabled", "true"))

	var surface bytes.Buffer
	closeLogs, err := setupTelemetryLogging(ctx, store, dir, &surface)
	require.NoError(t, err)
	probeDefaultSink(ctx, "teed_sink")
	closeLogs()

	require.Contains(t, surface.String(), "teed_sink")
	written, err := os.ReadFile(filepath.Join(dir, "telemetry.log"))
	require.NoError(t, err)
	require.Contains(t, string(written), "teed_sink")
}

// A surface that draws on the terminal passes its log file as dest, so the
// default handler must follow dest even with telemetry off — otherwise it keeps
// writing over the screen.
func TestUnit_SetupTelemetryLogging_TelemetryOffStillHonoursDest(t *testing.T) {
	ctx, _, store := setupSQLiteStore(t)
	dir := t.TempDir()
	restoreDefaultSink(t, ctx, store, dir)

	var surface bytes.Buffer
	closeLogs, err := setupTelemetryLogging(ctx, store, dir, &surface)
	require.NoError(t, err)
	probeDefaultSink(ctx, "dest_only_sink")
	closeLogs()

	require.Contains(t, surface.String(), "dest_only_sink")
	_, statErr := os.Stat(filepath.Join(dir, "telemetry.log"))
	require.True(t, os.IsNotExist(statErr), "telemetry.log must not be created while telemetry-enabled is off")
}

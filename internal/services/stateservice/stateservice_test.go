package stateservice

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/contenox/contenox/internal/store/runtimetypes"
	libdb "github.com/contenox/contenox/libdbexec"
	"github.com/stretchr/testify/require"
)

func newTestService(t *testing.T) Service {
	t.Helper()
	db, err := libdb.NewSQLiteDBManager(context.Background(), filepath.Join(t.TempDir(), "cli-config.db"), runtimetypes.SchemaSQLite)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	return New(nil, db, "")
}

func strPtr(s string) *string { return &s }

func TestSetCLIConfig_TelemetryAndUpdateCheckRoundTrip(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()

	snap, err := svc.SetCLIConfig(ctx, CLIConfigPatch{
		TelemetryEnabled: strPtr("true"),
		UpdateCheck:      strPtr("false"),
	})
	require.NoError(t, err)
	require.Equal(t, "true", snap.TelemetryEnabled)
	require.Equal(t, "false", snap.UpdateCheck)
	require.True(t, snap.Present["telemetry-enabled"])
	require.True(t, snap.Present["update-check"])

	snap, err = svc.CLIConfig(ctx)
	require.NoError(t, err)
	require.Equal(t, "true", snap.TelemetryEnabled)
	require.Equal(t, "false", snap.UpdateCheck)
}

func TestSetCLIConfig_DefaultThinkNormalizesLikeCLI(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()

	snap, err := svc.SetCLIConfig(ctx, CLIConfigPatch{DefaultThink: strPtr("LOW")})
	require.NoError(t, err)
	require.Equal(t, "low", snap.DefaultThink)

	// Empty clears the override so the runtime's own "high" fallback applies.
	snap, err = svc.SetCLIConfig(ctx, CLIConfigPatch{DefaultThink: strPtr("  ")})
	require.NoError(t, err)
	require.Equal(t, "", snap.DefaultThink)

	_, err = svc.SetCLIConfig(ctx, CLIConfigPatch{DefaultThink: strPtr("extreme")})
	require.Error(t, err)
}

// TestSetCLIConfig_MissionDefaultsRoundTrip pins the pair that makes `/mission
// <intent>` fireable without naming an agent: both keys are GLOBAL (no workspace
// scope — a mission is fired at the fleet, not at a project), both round-trip
// through the snapshot, and an empty value clears the setting rather than being
// ignored, so the UI can unset a default it once wrote.
func TestSetCLIConfig_MissionDefaultsRoundTrip(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()

	snap, err := svc.SetCLIConfig(ctx, CLIConfigPatch{
		DefaultMissionAgent:  strPtr("  chain-contenox  "),
		DefaultMissionPolicy: strPtr("hitl-policy-default.json"),
	})
	require.NoError(t, err)
	require.Equal(t, "chain-contenox", snap.DefaultMissionAgent, "the stored agent name is trimmed")
	require.Equal(t, "hitl-policy-default.json", snap.DefaultMissionPolicy)
	require.True(t, snap.Present["default-mission-agent"])
	require.True(t, snap.Present["default-mission-policy"])

	snap, err = svc.CLIConfig(ctx)
	require.NoError(t, err)
	require.Equal(t, "chain-contenox", snap.DefaultMissionAgent)
	require.Equal(t, "hitl-policy-default.json", snap.DefaultMissionPolicy)

	snap, err = svc.SetCLIConfig(ctx, CLIConfigPatch{DefaultMissionAgent: strPtr("")})
	require.NoError(t, err)
	require.Equal(t, "", snap.DefaultMissionAgent, "an explicit empty value clears the default")
	require.Equal(t, "hitl-policy-default.json", snap.DefaultMissionPolicy,
		"a nil field in the patch leaves its key untouched")
}

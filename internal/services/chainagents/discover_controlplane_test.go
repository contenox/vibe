package chainagents

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	libdb "github.com/contenox/beam/internal/libdbexec"
	"github.com/contenox/beam/internal/services/agentregistryservice"
	"github.com/contenox/beam/internal/services/vfs"
	"github.com/contenox/beam/internal/store/runtimetypes"
	"github.com/stretchr/testify/require"
)

// TestUnit_Discover_SurvivesControlPlaneDeny pins the regression the carveout
// slice shipped: discovery walks the .contenox dirs, which ARE the control
// plane, and the day the deny landed, serve boot logged "chainagents: list
// chains in …/.contenox: path is inside the runtime control plane" and
// declared NO chain agents — the primary fleet flow lost its agents. The fix
// is the privileged lane (localfileservice.NewPrivileged over
// vfs.OpenPrivilegedView): the runtime reading its OWN governing state is not
// the threat the invariant targets. This test registers the chain root as
// control-plane-denied and asserts discovery still declares the agent —
// while the agent-facing guarded path keeps refusing the same directory.
func TestUnit_Discover_SurvivesControlPlaneDeny(t *testing.T) {
	contenoxDir := t.TempDir()
	chain := map[string]any{
		"id": "agent-cp-regression",
		"tasks": []map[string]any{{
			"id": "reply", "handler": "noop", "print": "ok",
		}},
	}
	data, err := json.Marshal(chain)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(contenoxDir, "agent-cp-regression.json"), data, 0o600))

	require.NoError(t, vfs.SetControlPlaneDenied(contenoxDir))
	t.Cleanup(func() { require.NoError(t, vfs.SetControlPlaneDenied()) })

	// The agent-facing guard still refuses this directory — privilege is the
	// internal lane, not a hole.
	view, err := vfs.OpenView(filepath.Dir(contenoxDir))
	require.NoError(t, err)
	_, err = view.Resolve(filepath.Base(contenoxDir))
	require.ErrorIs(t, err, vfs.ErrControlPlane, "the guarded path must keep refusing the control plane")

	ctx := context.Background()
	db, err := libdb.NewSQLiteDBManager(ctx, filepath.Join(t.TempDir(), "reg.db"), runtimetypes.SchemaSQLite)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	registry := agentregistryservice.New(db)

	res, err := Discover(ctx, registry, contenoxDir)
	require.NoError(t, err, "discovery must read the runtime's own dirs regardless of the deny")
	require.Contains(t, res.Created, "agent-cp-regression")
}

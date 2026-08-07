package chainagents

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/contenox/contenox/internal/services/agentregistryservice"
	"github.com/contenox/contenox/internal/services/vfs"
	"github.com/contenox/contenox/internal/store/runtimetypes"
	libdb "github.com/contenox/contenox/libdbexec"
	"github.com/stretchr/testify/require"
)

// Discovery walks the .contenox dirs via the privileged lane, so it must
// still declare agents there even when that path is control-plane-denied
// for the ordinary agent-facing guard.
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
	require.NoError(t, os.WriteFile(filepath.Join(contenoxDir, "chain-agent-cp-regression.json"), data, 0o600))

	require.NoError(t, vfs.SetControlPlaneDenied(contenoxDir))
	t.Cleanup(func() { require.NoError(t, vfs.SetControlPlaneDenied()) })

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

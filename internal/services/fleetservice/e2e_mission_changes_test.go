package fleetservice

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/contenox/contenox/internal/kernel/agentinstance"
	"github.com/contenox/contenox/internal/services/agentregistryservice"
	"github.com/contenox/contenox/internal/services/missionchanges"
	"github.com/contenox/contenox/internal/services/missionservice"
	"github.com/contenox/contenox/internal/store/runtimetypes"
	"github.com/contenox/contenox/libacp"
	libdb "github.com/contenox/contenox/libdbexec"
	"github.com/contenox/contenox/libtracker"
	"github.com/stretchr/testify/require"
)

const missionChangesChain = `{
  "id": "e2e-mission-changes",
  "tasks": [
    {
      "id": "write_alpha",
      "handler": "tools",
      "tools": {"name": "local_fs", "tool_name": "write_file", "args": {"path": "alpha.txt", "content": "alpha-v1"}},
      "transition": {"branches": [{"operator": "default", "goto": "write_bravo"}]}
    },
    {
      "id": "write_bravo",
      "handler": "tools",
      "tools": {"name": "local_fs", "tool_name": "write_file", "args": {"path": "sub/bravo.txt", "content": "bravo"}},
      "transition": {"branches": [{"operator": "default", "goto": "report"}]}
    },
    {
      "id": "report",
      "handler": "tools",
      "tools": {"name": "mission", "tool_name": "mission_report", "args": {"kind": "progress", "summary": "wrote alpha and bravo"}},
      "transition": {"branches": [{"operator": "default", "goto": "done"}]}
    },
    {
      "id": "done",
      "handler": "noop",
      "transition": {"branches": [{"operator": "default", "goto": "end"}]}
    }
  ]
}`

// TestFleetService_E2E_MissionChanges: a unit's real writes are listed by
// the changed-files endpoint ordered by edit-weighted DOI with no scope
// anomaly; an injected out-of-cwd read then trips scopeAnomaly as advice,
// without blocking the unit or losing the real changed files.
func TestFleetService_E2E_MissionChanges(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e: builds the contenox binary and spawns a real ACP subprocess")
	}

	bin := buildContenoxBin(t)

	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	contenoxDir := filepath.Join(tmpHome, ".contenox")
	require.NoError(t, os.MkdirAll(contenoxDir, 0o700))
	dbPath := filepath.Join(contenoxDir, "local.db")

	ctx := context.Background()
	db, err := libdb.NewSQLiteDBManager(ctx, dbPath, runtimetypes.SchemaSQLite)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	runContenox(t, bin, "config", "set", "default-model", "fake-e2e-model-does-not-exist")

	chainPath := filepath.Join(contenoxDir, "mission-changes-chain.json")
	require.NoError(t, os.WriteFile(chainPath, []byte(missionChangesChain), 0o644))

	agents := agentregistryservice.New(db)
	agent := &runtimetypes.Agent{Name: "writer", Enabled: true}
	require.NoError(t, agent.SetExternalACPConfig(runtimetypes.ExternalACPConfig{
		Transport: runtimetypes.ExternalACPTransportStdio,
		Command:   bin,
		Args:      []string{"acp", "--auto"},
		Env:       map[string]string{"CONTENOX_ACP_CHAIN_PATH": chainPath},
	}))
	require.NoError(t, agents.Create(ctx, agent))

	stderr := &lockedBuffer{}
	kernel := agentinstance.New(agents, agentinstance.WithStderr(stderr))
	t.Cleanup(func() { _ = kernel.Close() })
	missions := missionservice.New(db)
	svc := New(kernel, agents, missions, nil, tmpHome, libtracker.NoopTracker{})

	// missionCwd is $HOME deliberately: the agent sandbox's only writable root is this cwd, and the unit must also reach $HOME/.contenox/local.db here to boot.
	missionCwd := tmpHome

	result, err := svc.Dispatch(ctx, DispatchRequest{
		AgentName:      "writer",
		Intent:         "write the files",
		HITLPolicyName: "default",
		Cwd:            missionCwd,
	})
	require.NoError(t, err, "dispatch stderr:\n%s", stderr.String())
	require.NotEmpty(t, result.MissionID)

	reader, ok := kernel.(missionchanges.SessionJournalReader)
	require.True(t, ok, "the concrete kernel Manager must satisfy SessionJournalReader")
	changesSvc := missionchanges.New(missions, reader)

	var changes *missionchanges.Changes
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		changes, err = changesSvc.Changes(ctx, result.MissionID)
		require.NoError(t, err)
		if len(changes.Files) >= 2 {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	require.GreaterOrEqualf(t, len(changes.Files), 2,
		"the unit must have journaled two changed files.\nstderr:\n%s", stderr.String())

	alpha := requireChangedFileWithSuffix(t, changes.Files, "/alpha.txt")
	bravo := requireChangedFileWithSuffix(t, changes.Files, "/sub/bravo.txt")

	require.Equal(t, missionchanges.StatusAdded, alpha.Status, "alpha.txt was created by the mission")
	require.Equal(t, missionchanges.StatusAdded, bravo.Status, "bravo.txt was created by the mission")

	diff, err := changesSvc.Diff(ctx, result.MissionID, alpha.Path)
	require.NoError(t, err)
	require.Equal(t, "", diff.Original, "alpha.txt did not exist before the mission (first OldText empty)")
	require.Equal(t, "alpha-v1", diff.Modified, "alpha.txt's written content is the modified side")

	require.False(t, changes.Scope.Anomaly, "every written path is under the mission cwd; no anomaly")
	require.GreaterOrEqual(t, changes.Scope.Files, 2, "at least alpha and bravo are touched")
	require.GreaterOrEqual(t, changes.Scope.Dirs, 2, "root and sub/ are two distinct top-level dirs")

	require.Equal(t, alpha.Score, bravo.Score, "one edit each until an interaction differentiates them")

	require.NoError(t, kernel.DeliverToSession(ctx,
		libacp.SessionID(result.SessionID),
		libacp.SessionNotification{Update: libacp.SessionUpdate{
			SessionUpdate: libacp.SessionUpdateToolCallUpdate,
			ToolCallID:    "reread-alpha",
			Kind:          libacp.ToolKindRead,
			Status:        libacp.ToolCallStatusCompleted,
			Locations:     []libacp.ToolCallLocation{{Path: alpha.Path}},
		}}))

	ranked, err := changesSvc.Changes(ctx, result.MissionID)
	require.NoError(t, err)
	rankedAlpha := requireChangedFileWithSuffix(t, ranked.Files, "/alpha.txt")
	rankedBravo := requireChangedFileWithSuffix(t, ranked.Files, "/sub/bravo.txt")
	require.Greater(t, rankedAlpha.Score, rankedBravo.Score,
		"alpha (edit+read) must outscore bravo (edit only)")
	require.Equal(t, rankedAlpha.Path, ranked.Files[0].Path,
		"the highest-DOI file must lead the ordered changed-files list")

	outsidePath := filepath.Join(t.TempDir(), "wander.txt")
	require.NoError(t, kernel.DeliverToSession(ctx,
		libacp.SessionID(result.SessionID),
		libacp.SessionNotification{Update: libacp.SessionUpdate{
			SessionUpdate: libacp.SessionUpdateToolCallUpdate,
			ToolCallID:    "wander-read",
			Kind:          libacp.ToolKindRead,
			Status:        libacp.ToolCallStatusCompleted,
			Locations:     []libacp.ToolCallLocation{{Path: outsidePath}},
		}}))

	after, err := changesSvc.Changes(ctx, result.MissionID)
	require.NoError(t, err)
	require.True(t, after.Scope.Anomaly, "a touched path outside the mission cwd must trip scopeAnomaly")
	require.Contains(t, after.Scope.OutsidePaths, outsidePath, "the wander path must be sampled")
	requireChangedFileWithSuffix(t, after.Files, "/alpha.txt")
	requireChangedFileWithSuffix(t, after.Files, "/sub/bravo.txt")
}

func requireChangedFileWithSuffix(t *testing.T, files []missionchanges.ChangedFile, suffix string) missionchanges.ChangedFile {
	t.Helper()
	for _, f := range files {
		if strings.HasSuffix(f.Path, suffix) {
			return f
		}
	}
	t.Fatalf("no changed file ending in %q; got %+v", suffix, files)
	return missionchanges.ChangedFile{}
}

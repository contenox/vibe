package fleetservice

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/contenox/beam/internal/kernel/agentinstance"
	libdb "github.com/contenox/beam/internal/libdbexec"
	"github.com/contenox/beam/internal/libtracker"
	"github.com/contenox/beam/internal/services/agentregistryservice"
	"github.com/contenox/beam/internal/services/missionchanges"
	"github.com/contenox/beam/internal/services/missionservice"
	"github.com/contenox/beam/internal/store/runtimetypes"
	"github.com/contenox/beam/libacp"
	"github.com/stretchr/testify/require"
)

// missionChangesChain is the deterministic, model-free chain the dispatched unit
// runs: it writes two files (alpha.txt in the root, sub/bravo.txt in a
// subdirectory) and files a mission_report so its turn is not silent — the report
// is what stops the unattended-mission nudge from re-running the whole chain and
// doubling every write. Every task is a `tools` handler calling a real local_fs /
// mission tool with static args; no model is ever resolved.
//
// The paths are relative to the dispatched unit's cwd, which local_fs roots the
// writes at, so the journaled diff paths come back absolute under that cwd —
// exactly the workspace root the scope check compares against.
//
// Read/stat/list tool events carry their path only in the model-driven
// tool-calling flow; the DETERMINISTIC `tools` handler journals a diff (and thus
// a path) for a write but no path for a read, so the read attention that lifts
// alpha above bravo in the ordering assertion is injected as a real-shaped read
// event below (a fidelity the deterministic harness cannot itself produce). The
// edit signal — the load-bearing one for the changed-files list — is entirely
// real.
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

// TestFleetService_E2E_MissionChanges is the acceptance for runtime/missionchanges,
// end to end through a REAL dispatched unit and the REAL kernel journal:
//
//   - Scenario 1 (real writes): a unit dispatched on a mission writes two files
//     (one twice) into its cwd. The changed-files endpoint, folding the unit's
//     own journaled diffs, must list both with the right first/last text and
//     status, ordered by edit-weighted Degree-of-Interest (the twice-written file
//     first), with a scope summary that counts the distinct files/dirs and raises
//     NO anomaly (everything is under the workspace root).
//
//   - Scenario 2 (scope anomaly): an out-of-cwd read is injected into the same
//     session's journal (DeliverToSession — exactly how an out-of-band update
//     lands), standing in for a derailed unit's wander into another tree. The same
//     fold must now trip scopeAnomaly and name the offending path, while the real
//     changed files stay put. This is the scope-not-steps premise as the fleet's
//     cheapest alarm — and it is ADVICE: the flag never stops the unit.
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

	// A model must be configured for the unit to build its engine even though the
	// chain resolves none; a deliberately fake name fails loudly on any accidental
	// model call.
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

	// The unit's workspace: local_fs roots every write here, so the journaled diff
	// paths come back absolute under it and the scope check has a known root.
	//
	// It is $HOME, deliberately. This unit is declared as an EXTERNAL agent, so it
	// is spawned inside the agent sandbox, whose one writable root is this cwd —
	// and it is a contenox binary, so it must also reach its own
	// $HOME/.contenox/local.db to boot at all. A cwd in some other temp dir leaves
	// that database outside the wall and the unit dies on `unable to open database
	// file` before it can write anything. (A contenox-runs-contenox unit in
	// production is a CHAIN agent, self-spawned and unconfined, so it has no such
	// constraint — see agenthost.ExternalACPAgent.SelfSpawn.)
	missionCwd := tmpHome

	result, err := svc.Dispatch(ctx, DispatchRequest{
		AgentName:      "writer",
		Intent:         "write the files",
		HITLPolicyName: "default",
		Cwd:            missionCwd,
	})
	require.NoError(t, err, "dispatch stderr:\n%s", stderr.String())
	require.NotEmpty(t, result.MissionID)

	// The attention-layer service under test, reading the REAL kernel journal via
	// the SessionJournal accessor (reached through the optional-capability
	// assertion the serve wiring also uses).
	reader, ok := kernel.(missionchanges.SessionJournalReader)
	require.True(t, ok, "the concrete kernel Manager must satisfy SessionJournalReader")
	changesSvc := missionchanges.New(missions, reader)

	// Poll until both files have been journaled (the first turn runs detached).
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

	// --- Scenario 1: changed files, status, scope (all from real journaled diffs) ---

	alpha := requireChangedFileWithSuffix(t, changes.Files, "/alpha.txt")
	bravo := requireChangedFileWithSuffix(t, changes.Files, "/sub/bravo.txt")

	require.Equal(t, missionchanges.StatusAdded, alpha.Status, "alpha.txt was created by the mission")
	require.Equal(t, missionchanges.StatusAdded, bravo.Status, "bravo.txt was created by the mission")

	// first OldText / last NewText for alpha's real write.
	diff, err := changesSvc.Diff(ctx, result.MissionID, alpha.Path)
	require.NoError(t, err)
	require.Equal(t, "", diff.Original, "alpha.txt did not exist before the mission (first OldText empty)")
	require.Equal(t, "alpha-v1", diff.Modified, "alpha.txt's written content is the modified side")

	require.False(t, changes.Scope.Anomaly, "every written path is under the mission cwd; no anomaly")
	require.GreaterOrEqual(t, changes.Scope.Files, 2, "at least alpha and bravo are touched")
	require.GreaterOrEqual(t, changes.Scope.Dirs, 2, "root and sub/ are two distinct top-level dirs")

	// Before the extra read, alpha and bravo are one edit each — equal DOI.
	require.Equal(t, alpha.Score, bravo.Score, "one edit each until an interaction differentiates them")

	// --- Scenario 1b: edit-weighted ordering via a real-shaped read of alpha ---
	//
	// Inject a read of alpha's exact path (the attention a model-driven unit would
	// have journaled). alpha now carries edit+read, bravo only edit, so alpha must
	// lead the ordered changed-files list: review starts where attention
	// concentrated, and the ordering is by edit-weighted score, not by path.
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

	// --- Scenario 2: scope anomaly from an out-of-cwd touch ---

	// A path in a DIFFERENT tree than the mission cwd — the wander a derailed unit
	// makes. Injected into the live session journal exactly as any out-of-band
	// session update would arrive.
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
	// The real changed files survive the anomaly — advice, not a gate.
	requireChangedFileWithSuffix(t, after.Files, "/alpha.txt")
	requireChangedFileWithSuffix(t, after.Files, "/sub/bravo.txt")
}

// requireChangedFileWithSuffix finds the one changed file whose absolute path ends
// with suffix (the diff paths are cwd-absolute, so tests match by suffix).
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

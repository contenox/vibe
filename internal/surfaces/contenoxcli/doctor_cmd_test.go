package contenoxcli

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/contenox/contenox/internal/models/runtimestate"
	"github.com/contenox/contenox/internal/services/missionservice"
	"github.com/contenox/contenox/internal/services/setupcheck"
	"github.com/contenox/contenox/internal/store/runtimetypes"
	"github.com/contenox/contenox/internal/substrate"
	"github.com/contenox/contenox/internal/version"
	libdb "github.com/contenox/contenox/libdbexec"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
)

// TestUnit_DoctorVerdict asserts doctor answers "can I chat right now" first:
// a single Ready line, and when not ready the highest-ranked blocking issue
// plus exactly one command to run next.
func TestUnit_DoctorVerdict(t *testing.T) {
	t.Run("ready says yes and names the one command to run next", func(t *testing.T) {
		res := setupcheck.Result{DefaultModel: "qwen3:8b", DefaultProvider: "ollama"}
		ready, reason, next := doctorVerdict(res)
		require.True(t, ready)
		require.Empty(t, reason)
		require.Empty(t, next)

		var out strings.Builder
		printDoctorVerdict(&out, res)
		require.Contains(t, out.String(), "Ready: yes — run: contenox beam",
			"a ready verdict must hand over a next step, not stop at the verdict")
	})

	t.Run("not ready names the ranked reason and its own fix", func(t *testing.T) {
		res := setupcheck.Result{Issues: []setupcheck.Issue{
			{Code: "default_model_not_available", Severity: "error", Message: "model not served", CLICommand: "contenox model list"},
			{Code: "missing_default_model", Severity: "error", Message: "no default model set", CLICommand: "contenox config set default-model qwen3:8b"},
		}}
		ready, reason, next := doctorVerdict(res)
		require.False(t, ready)
		require.Equal(t, "no default model set", reason, "the lowest issueRank blocker is the one to name")
		require.Equal(t, "contenox config set default-model qwen3:8b", next)

		var out strings.Builder
		printDoctorVerdict(&out, res)
		require.Contains(t, out.String(), "Ready: no — no default model set")
		require.Contains(t, out.String(), "Next:  contenox config set default-model qwen3:8b")
	})

	t.Run("a blocker with no command falls through to the next one that has it", func(t *testing.T) {
		res := setupcheck.Result{Issues: []setupcheck.Issue{
			{Code: "no_backends", Severity: "error", Message: "no backends registered"},
			{Code: "no_chat_models", Severity: "error", Message: "nothing to chat with", CLICommand: "ollama pull qwen3:8b"},
		}}
		_, reason, next := doctorVerdict(res)
		require.Equal(t, "no backends registered", reason)
		require.Equal(t, "ollama pull qwen3:8b", next)
	})

	t.Run("no blocker names its fix falls back to the wizard", func(t *testing.T) {
		res := setupcheck.Result{Issues: []setupcheck.Issue{
			{Code: "no_backends", Severity: "error", Message: "no backends registered"},
		}}
		_, _, next := doctorVerdict(res)
		require.Equal(t, doctorFallbackCommand, next)
	})

	t.Run("warnings alone do not make it not ready", func(t *testing.T) {
		res := setupcheck.Result{Issues: []setupcheck.Issue{
			{Code: "default_embed_model_missing", Severity: "warning", Message: "no embedding model"},
		}}
		ready, _, _ := doctorVerdict(res)
		require.True(t, ready, "Ready must stay the shared predicate, not a second definition")
	})

	t.Run("the verdict leads the text report", func(t *testing.T) {
		var out strings.Builder
		printDoctorText(&out, setupcheck.Result{DefaultModel: "m", DefaultProvider: "p"})
		lines := strings.SplitN(out.String(), "\n", 2)
		require.True(t, strings.HasPrefix(lines[0], "Ready: "), "got first line %q", lines[0])
	})
}

// TestUnit_VisionSummary asserts doctor's vision line lists vision-capable models, flags a text-only default, and stays silent with no reachable backend.
func TestUnit_VisionSummary(t *testing.T) {
	state := map[string]runtimestate.BackendRuntimeState{
		"b1": {
			ID: "b1", Name: "openai",
			PulledModels: []runtimestate.ModelPullStatus{
				{Model: "gpt-4o", CanChat: true, CanVision: true},
				{Model: "qwen3-4b", CanChat: true},
			},
		},
		"b2": {
			ID: "b2", Name: "broken", Error: "connection refused",
			PulledModels: []runtimestate.ModelPullStatus{
				{Model: "ghost-vlm", CanChat: true, CanVision: true},
			},
		},
	}

	t.Run("lists vision models and flags a text-only default", func(t *testing.T) {
		v := visionSummaryFromState(state, "qwen3-4b")
		require.True(t, v.reachable)
		require.Equal(t, []string{"gpt-4o"}, v.visionModels, "models on erroring backends must not count")
		require.True(t, v.defaultKnown)
		require.False(t, v.defaultHasVision)

		var out strings.Builder
		printVisionSummary(&out, v)
		require.Contains(t, out.String(), "1 model(s) accept images")
		require.Contains(t, out.String(), "gpt-4o")
		require.Contains(t, out.String(), "default model is text-only")
	})

	t.Run("vision-capable default gets no warning", func(t *testing.T) {
		v := visionSummaryFromState(state, "gpt-4o")
		var out strings.Builder
		printVisionSummary(&out, v)
		require.NotContains(t, out.String(), "text-only")
	})

	t.Run("no vision models teaches the refusal", func(t *testing.T) {
		v := visionSummaryFromState(map[string]runtimestate.BackendRuntimeState{
			"b1": {ID: "b1", PulledModels: []runtimestate.ModelPullStatus{{Model: "qwen3-4b", CanChat: true}}},
		}, "")
		var out strings.Builder
		printVisionSummary(&out, v)
		require.Contains(t, out.String(), "requests with images will be refused")
	})

	t.Run("no reachable backend prints nothing", func(t *testing.T) {
		v := visionSummaryFromState(map[string]runtimestate.BackendRuntimeState{
			"b2": {ID: "b2", Error: "down"},
		}, "")
		var out strings.Builder
		printVisionSummary(&out, v)
		require.Empty(t, out.String())
	})
}

// TestUnit_ReclaimAbandonedMissions asserts doctor's mission line: it reports
// what the sweep reclaimed and stays silent when it reclaimed nothing, so the
// mutation behind a diagnostic is never invisible.
func TestUnit_ReclaimAbandonedMissions(t *testing.T) {
	ctx := context.Background()
	db, err := libdb.NewSQLiteDBManager(ctx, filepath.Join(t.TempDir(), "doctor.db"), runtimetypes.SchemaSQLite)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	missions := missionservice.New(db)
	orphan := &missionservice.Mission{Intent: "left open by a dead host", AgentName: "unit", HITLPolicyName: "p.json"}
	require.NoError(t, missions.Create(ctx, orphan))
	frozen := time.Now().UTC().Add(-missionservice.StaleHeartbeatAfter - time.Hour)
	orphan.CreatedAt = frozen
	orphan.LastHeartbeat = &frozen
	require.NoError(t, missions.Update(ctx, orphan))

	reclaimed, err := reclaimAbandonedMissions(ctx, db, "")
	require.NoError(t, err)
	require.Equal(t, 1, reclaimed)

	got, err := missions.Get(ctx, orphan.ID)
	require.NoError(t, err)
	require.Equal(t, missionservice.StatusAbandoned, got.Status)

	var out strings.Builder
	printReclaimedMissions(&out, 1)
	require.Contains(t, out.String(), "1 reclaimed as abandoned")
	require.Contains(t, out.String(), "contenox mission list")

	out.Reset()
	printReclaimedMissions(&out, 0)
	require.Empty(t, out.String(), "a doctor run that reclaimed nothing says nothing")

	again, err := reclaimAbandonedMissions(ctx, db, "")
	require.NoError(t, err)
	require.Equal(t, 0, again, "and the second run finds nothing left to collect")

	out.Reset()
	printMissionSweepFailure(&out, nil)
	require.Empty(t, out.String())
	printMissionSweepFailure(&out, errors.New("bus refused"))
	require.Contains(t, out.String(), "sweep did not run (bus refused)")
	require.Contains(t, out.String(), "may still read as open")
}

func TestUnit_StateStorage_SaysNothingWhenEveryBackendIsTheLocalFile(t *testing.T) {
	var out strings.Builder
	printStateStorage(&out, []substrate.Status{
		{Substrate: substrate.StoreSubstrate, Backend: "SQLite", Target: "/home/you/.contenox/local.db"},
		{Substrate: substrate.BusSubstrate, Backend: "SQLite", Target: "/home/you/.contenox/local.db"},
		{Substrate: substrate.KVSubstrate, Backend: "SQLite", Target: "/home/you/.contenox/local.db"},
	})
	require.Empty(t, out.String(), "an install that opted into nothing must see the report it saw before")
	require.NoError(t, firstUnreachableSubstrate([]substrate.Status{
		{Substrate: substrate.StoreSubstrate, Backend: "SQLite", Target: "/home/you/.contenox/local.db"},
	}))
}

func TestUnit_StateStorage_NamesEachBackendAndWhetherARemoteOneAnswers(t *testing.T) {
	statuses := []substrate.Status{
		{Substrate: substrate.StoreSubstrate, Backend: "Postgres", Setting: substrate.PostgresURLEnv, Target: "postgres://contenox:xxxxx@db:5432/contenox"},
		{Substrate: substrate.BusSubstrate, Backend: "NATS", Setting: substrate.NATSURLEnv, Target: "nats://bus:4222", Err: errors.New("no servers available for connection")},
		{Substrate: substrate.KVSubstrate, Backend: "SQLite", Target: "/home/you/.contenox/local.db"},
	}

	var out strings.Builder
	printStateStorage(&out, statuses)
	require.Equal(t, `
State storage:
  • store: Postgres (postgres://contenox:xxxxx@db:5432/contenox, from CONTENOX_POSTGRES_URL)
    Status: reachable
  • message bus: NATS (nats://bus:4222, from CONTENOX_NATS_URL)
    Status: unreachable
    Error: no servers available for connection
    Hint: Start that server or unset CONTENOX_NATS_URL; while it is set contenox never falls back to the local database.
  • key-value cache: SQLite (/home/you/.contenox/local.db)
    Status: local file
`, out.String())

	err := firstUnreachableSubstrate(statuses)
	require.Error(t, err)
	require.Contains(t, err.Error(), substrate.NATSURLEnv)
	require.Contains(t, err.Error(), "no servers available for connection")
}

func unreachableStatuses() []substrate.Status {
	return []substrate.Status{
		{Substrate: substrate.BusSubstrate, Backend: "NATS", Setting: substrate.NATSURLEnv, Target: "nats://bus:4222", Err: errors.New("no servers available for connection")},
	}
}

func doctorBundleCmd(t *testing.T) (*cobra.Command, *strings.Builder, *strings.Builder, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "bundle.zip")
	cmd := &cobra.Command{Use: "doctor"}
	cmd.Flags().Bool("bundle", true, "")
	cmd.Flags().String("bundle-out", path, "")
	out, errOut := new(strings.Builder), new(strings.Builder)
	cmd.SetOut(out)
	cmd.SetErr(errOut)
	return cmd, out, errOut, path
}

// An unreachable substrate is exactly when someone reaches for --json or
// --bundle, so the early return must still emit what doctor holds instead of
// withholding the diagnostics that were asked for.
func TestUnit_Doctor_UnreachableSubstrateStillEmitsJSONAndBundle(t *testing.T) {
	statuses := unreachableStatuses()
	unreachable := firstUnreachableSubstrate(statuses)
	require.Error(t, unreachable)

	t.Run("json", func(t *testing.T) {
		cmd, out, errOut, path := doctorBundleCmd(t)
		err := reportUnreachableSubstrate(cmd, true, statuses, unreachable, t.TempDir(), "")
		require.ErrorIs(t, err, unreachable, "the run still fails; it just stops failing silently")

		var got setupcheck.Result
		require.NoError(t, json.Unmarshal([]byte(out.String()), &got), "stdout must stay one parseable payload")
		require.False(t, got.Ready())
		require.Len(t, got.Issues, 1)
		require.Equal(t, "substrate_unreachable", got.Issues[0].Code)
		require.Contains(t, got.Issues[0].Message, substrate.NATSURLEnv)

		require.FileExists(t, path)
		require.Contains(t, errOut.String(), "Bundle:", "the bundle notice belongs on stderr, off the JSON payload")
	})

	t.Run("text", func(t *testing.T) {
		cmd, out, _, path := doctorBundleCmd(t)
		err := reportUnreachableSubstrate(cmd, false, statuses, unreachable, t.TempDir(), "")
		require.ErrorIs(t, err, unreachable)
		require.Contains(t, out.String(), "Status: unreachable", "the storage report is what names the server that did not answer")
		require.Contains(t, out.String(), "Bundle:")
		require.FileExists(t, path)
	})
}

// TestUnit_PrintBuildProvenance pins I7's doctor half: a VCS-stamped build
// names its revision, dirty flag, and build time; an unstamped one still names
// its version.
func TestUnit_PrintBuildProvenance(t *testing.T) {
	var out strings.Builder
	printBuildProvenance(&out, "v0.41.0", version.Provenance{})
	require.Equal(t, "\nBuild: v0.41.0\n", out.String())

	out.Reset()
	printBuildProvenance(&out, "v0.41.0", version.Provenance{
		Revision: "41b11dd6", Dirty: true, Time: "2026-08-18T06:57:00Z",
	})
	require.Equal(t, "\nBuild: v0.41.0 (revision 41b11dd6 (working tree modified), built 2026-08-18T06:57:00Z)\n", out.String())
}

// TestUnit_ToolRoster_NamesEveryToolAndItsBacking pins doctor's roster: each
// tool of the composition `contenox acp` registers, with its origin — local,
// the client capability it needs, or the MCP server that serves it. Doctor
// holds no ACP client, so client-backed lines state requirements, never a
// live verdict.
func TestUnit_ToolRoster_NamesEveryToolAndItsBacking(t *testing.T) {
	var out strings.Builder
	printToolRoster(context.Background(), &out, acpRosterToolsets(true), []*runtimetypes.MCPServer{
		{Name: "github", Transport: "http", URL: "http://localhost:3000/mcp"},
		{Name: "acp-conn-1-fs", Transport: "stdio", Command: "npx"},
	})
	s := out.String()

	require.Contains(t, s, "read_file — local_fs — needs client capability fs.readTextFile\n")
	require.Contains(t, s, "read_file_range — local_fs — needs client capability fs.readTextFile\n")
	require.Contains(t, s, "write_file — local_fs — needs client capability fs.readTextFile+fs.writeTextFile\n")
	require.Contains(t, s, "edit_file — local_fs — needs client capability fs.readTextFile+fs.writeTextFile\n")
	require.Contains(t, s, "sed — local_fs — needs client capability fs.readTextFile+fs.writeTextFile\n")
	require.Contains(t, s, "local_shell — local_shell — needs client capability terminal\n")
	require.Contains(t, s, "mission — local (in-process); tools advertised per session role\n")
	require.Contains(t, s, "github — MCP server (http http://localhost:3000/mcp)")
	require.Contains(t, s, "acp-conn-1-fs — MCP server (session-scoped, supplied by an attached client)\n")
	require.NotContains(t, s, "granted", "doctor has no client; it states requirements, not verdicts")

	// The roster is what `contenox acp` registers, so it would read as the truth
	// for every shape unless it says where those two are absent.
	require.Contains(t, s, "local_fs, local_shell — not mounted under `contenox serve`")
	require.Contains(t, s, "every capability is an MCP server")
}

// TestUnit_ACPRosterToolsets_MatchesTheACPComposition pins that doctor's
// roster is the acpToolset composition itself, not a second list that can
// drift from it.
func TestUnit_ACPRosterToolsets_MatchesTheACPComposition(t *testing.T) {
	sets := acpRosterToolsets(true)
	for _, name := range []string{"local_fs", "local_shell", "mission"} {
		require.Contains(t, sets, name)
	}
}

package contenoxcli

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/contenox/contenox/internal/models/runtimestate"
	"github.com/contenox/contenox/internal/services/missionservice"
	"github.com/contenox/contenox/internal/services/setupcheck"
	"github.com/contenox/contenox/internal/store/runtimetypes"
	libdb "github.com/contenox/contenox/libdbexec"
	"github.com/stretchr/testify/require"
)

// TestUnit_DoctorVerdict asserts doctor answers "can I chat right now" first:
// a single Ready line, and when not ready the highest-ranked blocking issue
// plus exactly one command to run next.
func TestUnit_DoctorVerdict(t *testing.T) {
	t.Run("ready says yes and names how to chat", func(t *testing.T) {
		res := setupcheck.Result{DefaultModel: "qwen3:8b", DefaultProvider: "ollama"}
		ready, reason, next := doctorVerdict(res)
		require.True(t, ready)
		require.Empty(t, reason)
		require.Empty(t, next)

		var out strings.Builder
		printDoctorVerdict(&out, res)
		require.Contains(t, out.String(), "Ready: yes")
		require.Contains(t, out.String(), "contenox new")
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

	require.Equal(t, 1, reclaimAbandonedMissions(ctx, db, ""))

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

	require.Equal(t, 0, reclaimAbandonedMissions(ctx, db, ""), "and the second run finds nothing left to collect")
}

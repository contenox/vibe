package missiontools_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/contenox/beam/internal/kernel/taskengine"
	libdb "github.com/contenox/beam/internal/libdbexec"
	"github.com/contenox/beam/internal/services/missionservice"
	"github.com/contenox/beam/internal/services/missiontools"
	"github.com/contenox/beam/internal/store/runtimetypes"
	"github.com/stretchr/testify/require"
)

// setup gives a test a real sqlite-backed mission service plus one open mission
// to report against, returning the service and the mission id. Exercising the
// real store (sqlite, no subprocess) is cheap and catches the same validate()
// drift the store itself would reject.
func setup(t *testing.T) (context.Context, missionservice.Service, string) {
	t.Helper()
	ctx := context.Background()
	db, err := libdb.NewSQLiteDBManager(ctx, filepath.Join(t.TempDir(), "missiontools.db"), runtimetypes.SchemaSQLite)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	svc := missionservice.New(db)
	m := &missionservice.Mission{Intent: "ship the board", AgentName: "runner", HITLPolicyName: "default"}
	require.NoError(t, svc.Create(ctx, m))
	return ctx, svc, m.ID
}

func reportCall(kind, summary string) *taskengine.ToolsCall {
	return &taskengine.ToolsCall{
		Name:     missiontools.ToolsProviderName,
		ToolName: missiontools.ToolNameReport,
		Args:     map[string]string{"kind": kind, "summary": summary},
	}
}

// TestUnit_MissionTools_ReportFilesAgainstBoundMission proves the core of the
// slice: a report tool call on a mission-bound context lands in that mission's
// reports and stamps a heartbeat. This is exactly the seam the e2e proves end to
// end through a real subprocess.
func TestUnit_MissionTools_ReportFilesAgainstBoundMission(t *testing.T) {
	ctx, svc, missionID := setup(t)
	tools := missiontools.New(svc, nil)

	toolCtx := missiontools.WithMissionID(ctx, missionID)
	out, dt, err := tools.Exec(toolCtx, time.Now(), nil, false, reportCall("finding", "found the leak"))
	require.NoError(t, err)
	require.Equal(t, taskengine.DataTypeString, dt)
	require.Contains(t, out, "finding")

	reports, err := svc.ListReports(ctx, missionID, 10)
	require.NoError(t, err)
	require.Len(t, reports, 1)
	require.Equal(t, missionservice.ReportKindFinding, reports[0].Kind)
	require.Equal(t, "found the leak", reports[0].Summary)
	require.Equal(t, missionID, reports[0].MissionID)

	// The report stamped liveness.
	m, err := svc.Get(ctx, missionID)
	require.NoError(t, err)
	require.NotNil(t, m.LastHeartbeat, "filing a report is proof of life and heartbeats the mission")
}

// TestUnit_MissionTools_AbsentWithoutMission is the envelope-at-construction
// invariant: off a mission, the tools are neither listed nor executable. A unit
// that is not on a mission has nothing to call — it cannot forge a mission id.
func TestUnit_MissionTools_AbsentWithoutMission(t *testing.T) {
	ctx, svc, _ := setup(t)
	tools := missiontools.New(svc, nil)

	// Not exposed to a model.
	listed, err := tools.GetToolsForToolsByName(ctx, missiontools.ToolsProviderName)
	require.NoError(t, err)
	require.Empty(t, listed, "off a mission the tools are absent from the tool list")

	// Not executable via the deterministic path either.
	_, _, err = tools.Exec(ctx, time.Now(), nil, false, reportCall("progress", "should not run"))
	require.Error(t, err)
	require.Contains(t, err.Error(), "only available to a unit dispatched on a mission")
}

// TestUnit_MissionTools_ExposedOnMission is the positive half: on a mission ALL
// four mission tools ARE listed, so a chain that opted them in can drive them.
func TestUnit_MissionTools_ExposedOnMission(t *testing.T) {
	ctx, svc, missionID := setup(t)
	tools := missiontools.New(svc, nil)

	listed, err := tools.GetToolsForToolsByName(missiontools.WithMissionID(ctx, missionID), missiontools.ToolsProviderName)
	require.NoError(t, err)
	require.Len(t, listed, 4)
	names := make([]string, len(listed))
	for i, tool := range listed {
		names[i] = tool.Function.Name
	}
	require.ElementsMatch(t, []string{
		missiontools.ToolNameReport,
		missiontools.ToolNameAskAttention,
		missiontools.ToolNamePlan,
		missiontools.ToolNameFinish,
	}, names)
}

// TestUnit_MissionTools_ReportScopedToOwnMission proves a unit reports only
// against the mission bound into ITS context — the per-mission grant. Two
// missions, a context bound to the first, a report lands only on the first.
func TestUnit_MissionTools_ReportScopedToOwnMission(t *testing.T) {
	ctx, svc, missionA := setup(t)
	other := &missionservice.Mission{Intent: "other work", AgentName: "runner", HITLPolicyName: "default"}
	require.NoError(t, svc.Create(ctx, other))

	tools := missiontools.New(svc, nil)
	_, _, err := tools.Exec(missiontools.WithMissionID(ctx, missionA), time.Now(), nil, false, reportCall("progress", "mine"))
	require.NoError(t, err)

	a, err := svc.ListReports(ctx, missionA, 10)
	require.NoError(t, err)
	require.Len(t, a, 1)
	b, err := svc.ListReports(ctx, other.ID, 10)
	require.NoError(t, err)
	require.Empty(t, b, "a unit cannot report on a mission that is not its own")
}

// TestUnit_MissionTools_ReportDefaultsKind proves a summary-only report defaults
// to progress rather than erroring on the missing enum.
func TestUnit_MissionTools_ReportDefaultsKind(t *testing.T) {
	ctx, svc, missionID := setup(t)
	tools := missiontools.New(svc, nil)

	call := &taskengine.ToolsCall{
		Name:     missiontools.ToolsProviderName,
		ToolName: missiontools.ToolNameReport,
		Args:     map[string]string{"summary": "just checking in"},
	}
	_, _, err := tools.Exec(missiontools.WithMissionID(ctx, missionID), time.Now(), nil, false, call)
	require.NoError(t, err)

	reports, err := svc.ListReports(ctx, missionID, 10)
	require.NoError(t, err)
	require.Len(t, reports, 1)
	require.Equal(t, missionservice.ReportKindProgress, reports[0].Kind)
}

// TestUnit_MissionTools_ReportReadsModelArgs proves the model-driven shape
// (map[string]any input) is read, including refs as a JSON array.
func TestUnit_MissionTools_ReportReadsModelArgs(t *testing.T) {
	ctx, svc, missionID := setup(t)
	tools := missiontools.New(svc, nil)

	// The absolute ref must genuinely exist: this test is about ARG PARSING, and
	// a result claiming a nonexistent absolute path would (correctly) trip the
	// conclusion verification gate and land downgraded — verify_test.go's job.
	outLog := filepath.Join(t.TempDir(), "out.log")
	require.NoError(t, os.WriteFile(outLog, []byte("all green"), 0o644))

	input := map[string]any{
		"kind":    "result",
		"summary": "done",
		"detail":  "all green",
		"refs":    []any{"README.md", outLog},
	}
	call := &taskengine.ToolsCall{Name: missiontools.ToolsProviderName, ToolName: missiontools.ToolNameReport}
	_, _, err := tools.Exec(missiontools.WithMissionID(ctx, missionID), time.Now(), input, false, call)
	require.NoError(t, err)

	reports, err := svc.ListReports(ctx, missionID, 10)
	require.NoError(t, err)
	require.Len(t, reports, 1)
	require.Equal(t, missionservice.ReportKindResult, reports[0].Kind)
	require.Equal(t, "all green", reports[0].Detail)
	require.Equal(t, []string{"README.md", outLog}, reports[0].Refs)
}

// TestUnit_MissionTools_ReportReadsModelHandover proves the model-driven shape
// carries a nested typed hand-off object through to the stored report.
func TestUnit_MissionTools_ReportReadsModelHandover(t *testing.T) {
	ctx, svc, missionID := setup(t)
	tools := missiontools.New(svc, nil)

	input := map[string]any{
		"kind":    "result",
		"summary": "hot loop ported",
		"handover": map[string]any{
			"outcome":         "ported the hot loop; benchmarks pending",
			"artifacts":       []any{"src/hotloop.rs"},
			"handoverForNext": "pick up the benchmark harness",
			"caveats":         "SIMD path untested on aarch64",
		},
	}
	call := &taskengine.ToolsCall{Name: missiontools.ToolsProviderName, ToolName: missiontools.ToolNameReport}
	_, _, err := tools.Exec(missiontools.WithMissionID(ctx, missionID), time.Now(), input, false, call)
	require.NoError(t, err)

	reports, err := svc.ListReports(ctx, missionID, 10)
	require.NoError(t, err)
	require.Len(t, reports, 1)
	require.NotNil(t, reports[0].Handover)
	require.Equal(t, "ported the hot loop; benchmarks pending", reports[0].Handover.Outcome)
	require.Equal(t, []string{"src/hotloop.rs"}, reports[0].Handover.Artifacts)
	require.Equal(t, "pick up the benchmark harness", reports[0].Handover.HandoverForNext)
	require.Equal(t, "SIMD path untested on aarch64", reports[0].Handover.Caveats)
}

// TestUnit_MissionTools_ReportReadsDeterministicHandover proves the deterministic
// `tools` path — whose Args are map[string]string and cannot carry a nested
// object — reaches the hand-off as a JSON string, the shape a deterministic chain
// uses.
func TestUnit_MissionTools_ReportReadsDeterministicHandover(t *testing.T) {
	ctx, svc, missionID := setup(t)
	tools := missiontools.New(svc, nil)

	call := &taskengine.ToolsCall{
		Name:     missiontools.ToolsProviderName,
		ToolName: missiontools.ToolNameReport,
		Args: map[string]string{
			"kind":     "result",
			"summary":  "done",
			"handover": `{"outcome":"shipped","handoverForNext":"wire the inbox next"}`,
		},
	}
	_, _, err := tools.Exec(missiontools.WithMissionID(ctx, missionID), time.Now(), nil, false, call)
	require.NoError(t, err)

	reports, err := svc.ListReports(ctx, missionID, 10)
	require.NoError(t, err)
	require.Len(t, reports, 1)
	require.NotNil(t, reports[0].Handover)
	require.Equal(t, "shipped", reports[0].Handover.Outcome)
	require.Equal(t, "wire the inbox next", reports[0].Handover.HandoverForNext)
}

// TestUnit_MissionTools_ReportWithoutHandoverIsLegacy proves a report with no
// hand-off stores with a nil Handover — the absent hand-off is the norm.
func TestUnit_MissionTools_ReportWithoutHandoverIsLegacy(t *testing.T) {
	ctx, svc, missionID := setup(t)
	tools := missiontools.New(svc, nil)

	_, _, err := tools.Exec(missiontools.WithMissionID(ctx, missionID), time.Now(), nil, false, reportCall("progress", "no hand-off here"))
	require.NoError(t, err)

	reports, err := svc.ListReports(ctx, missionID, 10)
	require.NoError(t, err)
	require.Len(t, reports, 1)
	require.Nil(t, reports[0].Handover, "a report with no hand-off carries a nil Handover")
}

// TestUnit_MissionTools_ReportRejectsMalformedHandover proves a syntactically
// broken hand-off object surfaces a legible error the model can correct, rather
// than being silently dropped.
func TestUnit_MissionTools_ReportRejectsMalformedHandover(t *testing.T) {
	ctx, svc, missionID := setup(t)
	tools := missiontools.New(svc, nil)

	call := &taskengine.ToolsCall{
		Name:     missiontools.ToolsProviderName,
		ToolName: missiontools.ToolNameReport,
		Args:     map[string]string{"kind": "result", "summary": "done", "handover": `{not json`},
	}
	_, _, err := tools.Exec(missiontools.WithMissionID(ctx, missionID), time.Now(), nil, false, call)
	require.Error(t, err)
	require.Contains(t, err.Error(), "handover")
}

// TestUnit_MissionTools_InvalidKindRejected proves a malformed kind still fails
// loudly through the store's own validation rather than being silently coerced.
func TestUnit_MissionTools_InvalidKindRejected(t *testing.T) {
	ctx, svc, missionID := setup(t)
	tools := missiontools.New(svc, nil)

	_, _, err := tools.Exec(missiontools.WithMissionID(ctx, missionID), time.Now(), nil, false, reportCall("gossip", "nope"))
	require.Error(t, err)
	require.Contains(t, err.Error(), "invalid report kind")
}

// fakeAsker records RaiseAttention calls so the wired attention path can be
// asserted without pulling in the full hitlservice.
type fakeAsker struct {
	missionID string
	summary   string
	detail    string
	calls     int
	// answer is what the operator replies; err makes the ask go unanswered.
	answer string
	err    error
}

func (f *fakeAsker) RaiseAttention(_ context.Context, missionID, summary, detail string) (string, error) {
	f.calls++
	f.missionID = missionID
	f.summary = summary
	f.detail = detail
	return f.answer, f.err
}

// TestUnit_MissionTools_AskAttentionUsesAskerWhenWired proves mission_ask_attention
// routes to the injected durable-ask machinery, scoped to the caller's mission.
func TestUnit_MissionTools_AskAttentionUsesAskerWhenWired(t *testing.T) {
	ctx, svc, missionID := setup(t)
	asker := &fakeAsker{answer: "staging, and use the seeded fixtures"}
	tools := missiontools.New(svc, asker)

	call := &taskengine.ToolsCall{
		Name:     missiontools.ToolsProviderName,
		ToolName: missiontools.ToolNameAskAttention,
		Args:     map[string]string{"summary": "need a decision", "detail": "prod or staging?"},
	}
	out, _, err := tools.Exec(missiontools.WithMissionID(ctx, missionID), time.Now(), nil, false, call)
	require.NoError(t, err)
	// THE POINT of asking: the operator's words come back as the tool result, so
	// the unit can act on them in the same turn instead of merely learning that
	// somebody was notified.
	require.Equal(t, "staging, and use the seeded fixtures", out)
	require.Equal(t, 1, asker.calls)
	require.Equal(t, missionID, asker.missionID)
	require.Equal(t, "need a decision", asker.summary)
	require.Equal(t, "prod or staging?", asker.detail)

	// No asker involvement means no report either.
	reports, err := svc.ListReports(ctx, missionID, 10)
	require.NoError(t, err)
	require.Empty(t, reports, "a wired asker does not double-write a blocker report")
}

// TestUnit_MissionTools_AskAttentionFallsBackToBlocker proves that with no asker
// wired, an attention request is not dropped — it lands as a durable blocker
// report (same store, no parallel mechanism).
func TestUnit_MissionTools_AskAttentionFallsBackToBlocker(t *testing.T) {
	ctx, svc, missionID := setup(t)
	tools := missiontools.New(svc, nil)

	call := &taskengine.ToolsCall{
		Name:     missiontools.ToolsProviderName,
		ToolName: missiontools.ToolNameAskAttention,
		Args:     map[string]string{"summary": "need a decision"},
	}
	_, _, err := tools.Exec(missiontools.WithMissionID(ctx, missionID), time.Now(), nil, false, call)
	require.NoError(t, err)

	reports, err := svc.ListReports(ctx, missionID, 10)
	require.NoError(t, err)
	require.Len(t, reports, 1)
	require.Equal(t, missionservice.ReportKindBlocker, reports[0].Kind)
	require.Equal(t, "need a decision", reports[0].Summary)
}

// TestUnit_MissionTools_UnknownMissionSurfacesError proves a report against a
// context bound to a nonexistent mission surfaces the store's not-found rather
// than a silent insert — the AddReport contract, exercised through the tool.
func TestUnit_MissionTools_UnknownMissionSurfacesError(t *testing.T) {
	ctx, svc, _ := setup(t)
	tools := missiontools.New(svc, nil)

	_, _, err := tools.Exec(missiontools.WithMissionID(ctx, "no-such-mission"), time.Now(), nil, false, reportCall("progress", "ghost"))
	require.Error(t, err)
	require.ErrorIs(t, err, libdb.ErrNotFound)
}

// TestUnit_MissionTools_MetaRoundTrip pins the session/new wire contract the
// dispatcher and the unit agree on: a mission id marshalled into `_meta` parses
// back out, and unrelated `_meta` reads as "not on a mission".
func TestUnit_MissionTools_MetaRoundTrip(t *testing.T) {
	raw := missionservice.MarshalMissionMeta("mission-123")
	require.NotNil(t, raw)
	id, ok := missionservice.ParseMissionMeta(raw)
	require.True(t, ok)
	require.Equal(t, "mission-123", id)

	_, ok = missionservice.ParseMissionMeta([]byte(`{"contenox.agent":"runner"}`))
	require.False(t, ok, "an unrelated _meta is not a mission")

	require.Nil(t, missionservice.MarshalMissionMeta("  "), "a blank id marshals to no _meta")
}

// planModelCall builds a mission_plan call in the MODEL-driven shape: the entries
// arrive as a []any of objects under a map[string]any `input`. It returns the
// call (with only the names set) and the input the transport hands to Exec.
func planModelCall(explanation string, entries ...map[string]any) (*taskengine.ToolsCall, map[string]any) {
	items := make([]any, len(entries))
	for i, e := range entries {
		items[i] = e
	}
	input := map[string]any{"entries": items}
	if explanation != "" {
		input["explanation"] = explanation
	}
	return &taskengine.ToolsCall{Name: missiontools.ToolsProviderName, ToolName: missiontools.ToolNamePlan}, input
}

func planEntryArg(content, status, priority string) map[string]any {
	return map[string]any{"content": content, "status": status, "priority": priority}
}

// TestUnit_MissionTools_PlanSetsSnapshotAndEchoesIDs is the core of the plan
// slice: a mission_plan call on a mission-bound context replaces the mission's
// plan with the snapshot, stamps a heartbeat, and echoes the STORED plan back —
// with the ids SetPlan assigned to the id-less entries, so the planner can carry
// them forward.
func TestUnit_MissionTools_PlanSetsSnapshotAndEchoesIDs(t *testing.T) {
	ctx, svc, missionID := setup(t)
	tools := missiontools.New(svc, nil)

	call, input := planModelCall("first cut",
		planEntryArg("survey the codebase", "in_progress", "high"),
		planEntryArg("port the hot loop", "pending", "medium"),
	)
	out, dt, err := tools.Exec(missiontools.WithMissionID(ctx, missionID), time.Now(), input, false, call)
	require.NoError(t, err)
	require.Equal(t, taskengine.DataTypeJSON, dt)

	// The result echoes the stored snapshot as a missionservice.Plan, with ids.
	echoed, ok := out.(missionservice.Plan)
	require.True(t, ok, "mission_plan echoes the stored Plan so ids carry forward")
	require.Equal(t, 1, echoed.Revision)
	require.Len(t, echoed.Entries, 2)
	require.NotEmpty(t, echoed.Entries[0].ID)
	require.NotEmpty(t, echoed.Entries[1].ID)
	require.Equal(t, "first cut", echoed.Explanation)

	// The plan is persisted on the mission.
	m, err := svc.Get(ctx, missionID)
	require.NoError(t, err)
	require.Equal(t, 1, m.Plan.Revision)
	require.Len(t, m.Plan.Entries, 2)
	require.Equal(t, "survey the codebase", m.Plan.Entries[0].Content)
	require.Equal(t, missionservice.PlanEntryInProgress, m.Plan.Entries[0].Status)
	require.Equal(t, missionservice.PlanEntryPriorityHigh, m.Plan.Entries[0].Priority)

	// Revising the plan stamped liveness.
	require.NotNil(t, m.LastHeartbeat, "revising the plan is proof of life and heartbeats the mission")
}

// TestUnit_MissionTools_PlanAbsentOffMission is the envelope-at-construction
// invariant for the plan tool: off a mission it refuses to execute, so a unit not
// on a mission cannot write a plan against a mission it names.
func TestUnit_MissionTools_PlanAbsentOffMission(t *testing.T) {
	ctx, svc, _ := setup(t)
	tools := missiontools.New(svc, nil)

	call, input := planModelCall("", planEntryArg("do a thing", "pending", "low"))
	_, _, err := tools.Exec(ctx, time.Now(), input, false, call)
	require.Error(t, err)
	require.Contains(t, err.Error(), "only available to a unit dispatched on a mission")
}

// TestUnit_MissionTools_PlanScopedToOwnMission proves a unit plans only against
// the mission bound into ITS context — the per-mission grant.
func TestUnit_MissionTools_PlanScopedToOwnMission(t *testing.T) {
	ctx, svc, missionA := setup(t)
	other := &missionservice.Mission{Intent: "other work", AgentName: "runner", HITLPolicyName: "default"}
	require.NoError(t, svc.Create(ctx, other))

	tools := missiontools.New(svc, nil)
	call, input := planModelCall("mine", planEntryArg("only on A", "pending", "high"))
	_, _, err := tools.Exec(missiontools.WithMissionID(ctx, missionA), time.Now(), input, false, call)
	require.NoError(t, err)

	a, err := svc.Get(ctx, missionA)
	require.NoError(t, err)
	require.Equal(t, 1, a.Plan.Revision)
	b, err := svc.Get(ctx, other.ID)
	require.NoError(t, err)
	require.Equal(t, 0, b.Plan.Revision, "a unit cannot plan on a mission that is not its own")
}

// TestUnit_MissionTools_PlanReadsDeterministicJSONArgs proves the deterministic
// `tools` path — whose Args are map[string]string and cannot carry a nested list
// — reaches the entries as a JSON string. This is the shape the e2e chain uses.
func TestUnit_MissionTools_PlanReadsDeterministicJSONArgs(t *testing.T) {
	ctx, svc, missionID := setup(t)
	tools := missiontools.New(svc, nil)

	call := &taskengine.ToolsCall{
		Name:     missiontools.ToolsProviderName,
		ToolName: missiontools.ToolNamePlan,
		Args: map[string]string{
			"entries":     `[{"content":"step one","status":"in_progress","priority":"high"},{"content":"step two","status":"pending","priority":"low"}]`,
			"explanation": "seed from a deterministic chain",
		},
	}
	// input is the flowing chain value in this path, not the args; nil is fine.
	_, _, err := tools.Exec(missiontools.WithMissionID(ctx, missionID), time.Now(), nil, false, call)
	require.NoError(t, err)

	m, err := svc.Get(ctx, missionID)
	require.NoError(t, err)
	require.Len(t, m.Plan.Entries, 2)
	require.Equal(t, "step one", m.Plan.Entries[0].Content)
	require.Equal(t, missionservice.PlanEntryInProgress, m.Plan.Entries[0].Status)
	require.Equal(t, "seed from a deterministic chain", m.Plan.Explanation)
}

// TestUnit_MissionTools_PlanRejectsEmptySnapshot proves an empty snapshot fails
// loudly through SetPlan's shape validation rather than silently erasing the
// plan (full-snapshot replace has no "erase" path — deletion is omission of
// individual entries).
func TestUnit_MissionTools_PlanRejectsEmptySnapshot(t *testing.T) {
	ctx, svc, missionID := setup(t)
	tools := missiontools.New(svc, nil)

	call := &taskengine.ToolsCall{Name: missiontools.ToolsProviderName, ToolName: missiontools.ToolNamePlan}
	input := map[string]any{"entries": []any{}}
	_, _, err := tools.Exec(missiontools.WithMissionID(ctx, missionID), time.Now(), input, false, call)
	require.Error(t, err)
	require.Contains(t, err.Error(), "at least one entry")
}

// TestUnit_MissionTools_PlanRejectsUnknownStatus proves a malformed enum surfaces
// through the store's validation — the tool stays hard on shape and does not
// coerce.
func TestUnit_MissionTools_PlanRejectsUnknownStatus(t *testing.T) {
	ctx, svc, missionID := setup(t)
	tools := missiontools.New(svc, nil)

	call, input := planModelCall("", planEntryArg("bad status", "halfway", "high"))
	_, _, err := tools.Exec(missiontools.WithMissionID(ctx, missionID), time.Now(), input, false, call)
	require.Error(t, err)
	require.Contains(t, err.Error(), "invalid plan entry status")
}

// TestUnit_MissionTools_PlanMissingEntriesRejected proves a plan call with no
// entries argument at all is rejected with a clear message, not a nil-plan write.
func TestUnit_MissionTools_PlanMissingEntriesRejected(t *testing.T) {
	ctx, svc, missionID := setup(t)
	tools := missiontools.New(svc, nil)

	call := &taskengine.ToolsCall{Name: missiontools.ToolsProviderName, ToolName: missiontools.ToolNamePlan}
	_, _, err := tools.Exec(missiontools.WithMissionID(ctx, missionID), time.Now(), map[string]any{}, false, call)
	require.Error(t, err)
	require.Contains(t, err.Error(), "requires an 'entries' array")
}

// TestUnit_MissionTools_PlanCarriesIDsForwardAndGuardsCompleted proves the id
// echo is load-bearing: a second revision that carries an entry's echoed id
// forward keeps its identity, and once that entry is completed its content is
// immutable — a later revision rewriting the completed entry's text (same id) is
// rejected, exactly the audit-safety guard the id carry-forward enables.
func TestUnit_MissionTools_PlanCarriesIDsForwardAndGuardsCompleted(t *testing.T) {
	ctx, svc, missionID := setup(t)
	tools := missiontools.New(svc, nil)
	mctx := missiontools.WithMissionID(ctx, missionID)

	// Rev 1: one id-less entry, in progress.
	call, input := planModelCall("start", planEntryArg("wire the seam", "in_progress", "high"))
	out, _, err := tools.Exec(mctx, time.Now(), input, false, call)
	require.NoError(t, err)
	id := out.(missionservice.Plan).Entries[0].ID
	require.NotEmpty(t, id)

	// Rev 2: carry the id forward, mark it completed with UNCHANGED content — fine.
	call, input = planModelCall("done that step", map[string]any{
		"id": id, "content": "wire the seam", "status": "completed", "priority": "high",
	})
	out, _, err = tools.Exec(mctx, time.Now(), input, false, call)
	require.NoError(t, err)
	require.Equal(t, 2, out.(missionservice.Plan).Revision)
	require.Equal(t, id, out.(missionservice.Plan).Entries[0].ID, "the entry keeps its identity across revisions")

	// Rev 3: rewrite the COMPLETED entry's content (same id) — the immutability
	// guard rejects it; corrections must be appended as new entries.
	call, input = planModelCall("oops", map[string]any{
		"id": id, "content": "wire the seam differently", "status": "completed", "priority": "high",
	})
	_, _, err = tools.Exec(mctx, time.Now(), input, false, call)
	require.Error(t, err)
	require.Contains(t, err.Error(), "already-completed work")
}

// finishCall builds a mission_finish call in the deterministic Args shape.
func finishCall(status, reason string) *taskengine.ToolsCall {
	return &taskengine.ToolsCall{
		Name:     missiontools.ToolsProviderName,
		ToolName: missiontools.ToolNameFinish,
		Args:     map[string]string{"status": status, "reason": reason},
	}
}

// TestUnit_MissionTools_FinishSetsTerminalStatus proves a mission_finish call
// moves the mission to the named terminal state, records the reason, and stamps a
// heartbeat.
func TestUnit_MissionTools_FinishSetsTerminalStatus(t *testing.T) {
	ctx, svc, missionID := setup(t)
	tools := missiontools.New(svc, nil)

	out, dt, err := tools.Exec(missiontools.WithMissionID(ctx, missionID), time.Now(), nil, false, finishCall("landed", "shipped it"))
	require.NoError(t, err)
	require.Equal(t, taskengine.DataTypeString, dt)
	require.Contains(t, out, "landed")

	m, err := svc.Get(ctx, missionID)
	require.NoError(t, err)
	require.Equal(t, missionservice.StatusLanded, m.Status)
	require.Equal(t, "shipped it", m.StatusReason)
	require.NotNil(t, m.LastHeartbeat, "finishing a mission heartbeats it")
}

// TestUnit_MissionTools_FinishReadsModelStuck proves the model-driven shape and a
// stuck verdict both work — stuck is a first-class terminal signal.
func TestUnit_MissionTools_FinishReadsModelStuck(t *testing.T) {
	ctx, svc, missionID := setup(t)
	tools := missiontools.New(svc, nil)

	call := &taskengine.ToolsCall{Name: missiontools.ToolsProviderName, ToolName: missiontools.ToolNameFinish}
	input := map[string]any{"status": "stuck", "reason": "cannot decide alone"}
	_, _, err := tools.Exec(missiontools.WithMissionID(ctx, missionID), time.Now(), input, false, call)
	require.NoError(t, err)

	m, err := svc.Get(ctx, missionID)
	require.NoError(t, err)
	require.Equal(t, missionservice.StatusStuck, m.Status)
}

// TestUnit_MissionTools_FinishRequiresStatus proves a finish call with no status
// is rejected before it reaches the store.
func TestUnit_MissionTools_FinishRequiresStatus(t *testing.T) {
	ctx, svc, missionID := setup(t)
	tools := missiontools.New(svc, nil)

	_, _, err := tools.Exec(missiontools.WithMissionID(ctx, missionID), time.Now(), nil, false, finishCall("", "no status"))
	require.Error(t, err)
	require.Contains(t, err.Error(), "requires a status")
}

// TestUnit_MissionTools_FinishRejectsNonTerminal proves a non-terminal target
// (open) is refused by Finish's guard, surfaced through the tool.
func TestUnit_MissionTools_FinishRejectsNonTerminal(t *testing.T) {
	ctx, svc, missionID := setup(t)
	tools := missiontools.New(svc, nil)

	_, _, err := tools.Exec(missiontools.WithMissionID(ctx, missionID), time.Now(), nil, false, finishCall("open", "not terminal"))
	require.Error(t, err)
	require.Contains(t, err.Error(), "terminal status is required")
}

// TestUnit_MissionTools_FinishIdempotentThenConflict proves the guard's two
// deliberate calls, both surfaced through the tool: a repeat of the SAME terminal
// status is an idempotent no-op, while a DIFFERENT terminal status over an
// already-finished mission is a conflict.
func TestUnit_MissionTools_FinishIdempotentThenConflict(t *testing.T) {
	ctx, svc, missionID := setup(t)
	tools := missiontools.New(svc, nil)
	mctx := missiontools.WithMissionID(ctx, missionID)

	_, _, err := tools.Exec(mctx, time.Now(), nil, false, finishCall("landed", "done"))
	require.NoError(t, err)

	// Same status again: idempotent, no error.
	_, _, err = tools.Exec(mctx, time.Now(), nil, false, finishCall("landed", "done again"))
	require.NoError(t, err)

	// Different terminal status over a finished mission: conflict.
	_, _, err = tools.Exec(mctx, time.Now(), nil, false, finishCall("derailed", "reversal"))
	require.Error(t, err)
	require.Contains(t, err.Error(), "already finished")
}

// TestUnit_MissionTools_FinishAbsentOffMission is the envelope-at-construction
// invariant for the finish tool.
func TestUnit_MissionTools_FinishAbsentOffMission(t *testing.T) {
	ctx, svc, _ := setup(t)
	tools := missiontools.New(svc, nil)

	_, _, err := tools.Exec(ctx, time.Now(), nil, false, finishCall("landed", "should not run"))
	require.Error(t, err)
	require.Contains(t, err.Error(), "only available to a unit dispatched on a mission")
}

// TestUnit_MissionTools_AskAttentionFallsBackWhenUnanswered pins the degradation
// that keeps a question from being lost: an asker that reports no answer (nobody
// replied inside the deadline, or the reply was a refusal) must still leave the
// question durably recorded as a blocker — and say WHY it is one, so the operator
// reading it knows an answer was solicited and missed, not never asked for.
func TestUnit_MissionTools_AskAttentionFallsBackWhenUnanswered(t *testing.T) {
	ctx, svc, missionID := setup(t)
	asker := &fakeAsker{err: errors.New("attention ask went unanswered")}
	tools := missiontools.New(svc, asker)

	call := &taskengine.ToolsCall{
		Name:     missiontools.ToolsProviderName,
		ToolName: missiontools.ToolNameAskAttention,
		Args:     map[string]string{"summary": "which project?", "detail": "the intent named none"},
	}
	out, _, err := tools.Exec(missiontools.WithMissionID(ctx, missionID), time.Now(), nil, false, call)
	require.NoError(t, err, "an unanswered ask is a fallback, never a tool failure")
	require.Contains(t, out, "no operator answered")

	reports, err := svc.ListReports(ctx, missionID, 10)
	require.NoError(t, err)
	require.Len(t, reports, 1, "the question survives as a durable blocker")
	require.Equal(t, missionservice.ReportKindBlocker, reports[0].Kind)
	require.Equal(t, "which project?", reports[0].Summary)
	require.Contains(t, reports[0].Detail, "the intent named none", "the original detail is kept")
	require.Contains(t, reports[0].Detail, "got no answer", "…and says an answer was solicited and missed")
}

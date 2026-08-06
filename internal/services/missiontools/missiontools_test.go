package missiontools_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/contenox/contenox/internal/kernel/taskengine"
	libdb "github.com/contenox/contenox/internal/libdbexec"
	"github.com/contenox/contenox/internal/services/missionservice"
	"github.com/contenox/contenox/internal/services/missiontools"
	"github.com/contenox/contenox/internal/store/runtimetypes"
	"github.com/stretchr/testify/require"
)

// setup gives a test a real sqlite-backed mission service plus one open mission to report against.
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

// TestUnit_MissionTools_ReportFilesAgainstBoundMission pins that a report call lands in the bound mission's reports and stamps a heartbeat.
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

	m, err := svc.Get(ctx, missionID)
	require.NoError(t, err)
	require.NotNil(t, m.LastHeartbeat, "filing a report is proof of life and heartbeats the mission")
}

// TestUnit_MissionTools_AbsentWithoutMission pins that off a mission the tools are neither listed nor executable.
func TestUnit_MissionTools_AbsentWithoutMission(t *testing.T) {
	ctx, svc, _ := setup(t)
	tools := missiontools.New(svc, nil)

	listed, err := tools.GetToolsForToolsByName(ctx, missiontools.ToolsProviderName)
	require.NoError(t, err)
	require.Empty(t, listed, "off a mission the tools are absent from the tool list")

	_, _, err = tools.Exec(ctx, time.Now(), nil, false, reportCall("progress", "should not run"))
	require.Error(t, err)
	require.Contains(t, err.Error(), "only available to a unit dispatched on a mission")
}

// TestUnit_MissionTools_ExposedOnMission pins that on a mission all four mission tools are listed.
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

// TestUnit_MissionTools_ReportScopedToOwnMission pins that a unit reports only against the mission bound into its context.
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

// TestUnit_MissionTools_ReportDefaultsKind pins that a summary-only report defaults to progress.
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

	// Neither the enum nor the required set can carry a default, so the
	// descriptor states it — and the published request schema, which is that
	// descriptor rendered, therefore states it too.
	declared, err := tools.GetToolsForToolsByName(missiontools.WithMissionID(ctx, missionID), missiontools.ToolsProviderName)
	require.NoError(t, err)
	var params map[string]any
	for _, tool := range declared {
		if tool.Function.Name == missiontools.ToolNameReport {
			params = tool.Function.Parameters.(map[string]any)
		}
	}
	require.NotNil(t, params, "mission_report is declared on a mission")
	require.NotContains(t, params["required"], "kind", "kind stays optional; the default is what makes that safe")
	kind := params["properties"].(map[string]any)["kind"].(map[string]any)
	require.Contains(t, kind["description"], "Omitted or blank files the report as progress.",
		"the default Exec applies must be in the description, since no error would ever teach it")
}

// TestUnit_MissionTools_ReportReadsModelArgs pins that the model-driven map[string]any input is read, including refs as a JSON array.
func TestUnit_MissionTools_ReportReadsModelArgs(t *testing.T) {
	ctx, svc, missionID := setup(t)
	tools := missiontools.New(svc, nil)

	// The absolute ref must genuinely exist: this test is about arg parsing, not
	// the conclusion verification gate (verify_test.go's job).
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

// TestUnit_MissionTools_ReportReadsModelHandover pins that the model-driven shape carries a nested typed hand-off through to storage.
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

// TestUnit_MissionTools_ReportReadsDeterministicHandover pins that the deterministic Args path reaches the hand-off as a JSON string.
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

// TestUnit_MissionTools_ReportWithoutHandoverIsLegacy pins that a report with no hand-off stores with a nil Handover.
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

// TestUnit_MissionTools_ReportRejectsMalformedHandover pins that a broken hand-off object surfaces a legible error, not a silent drop.
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

// TestUnit_MissionTools_InvalidKindRejected pins that a malformed kind fails loudly through the store's validation.
func TestUnit_MissionTools_InvalidKindRejected(t *testing.T) {
	ctx, svc, missionID := setup(t)
	tools := missiontools.New(svc, nil)

	_, _, err := tools.Exec(missiontools.WithMissionID(ctx, missionID), time.Now(), nil, false, reportCall("gossip", "nope"))
	require.Error(t, err)
	require.Contains(t, err.Error(), "invalid report kind")
}

// fakeAsker records RaiseAttention calls so the wired attention path can be asserted without pulling in hitlservice.
type fakeAsker struct {
	missionID string
	summary   string
	detail    string
	lastAsk   missiontools.AttentionAsk
	calls     int
	// answer is what the operator replies; err makes the ask go unanswered.
	answer string
	err    error
}

func (f *fakeAsker) RaiseAttention(_ context.Context, ask missiontools.AttentionAsk) (string, error) {
	f.calls++
	f.missionID = ask.MissionID
	f.summary = ask.Summary
	f.detail = ask.Detail
	f.lastAsk = ask
	return f.answer, f.err
}

// TestUnit_MissionTools_AskAttentionUsesAskerWhenWired pins that mission_ask_attention routes to the injected asker, scoped to the mission.
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
	require.Equal(t, "staging, and use the seeded fixtures", out, "the operator's words come back as the tool result")
	require.Equal(t, 1, asker.calls)
	require.Equal(t, missionID, asker.missionID)
	require.Equal(t, "need a decision", asker.summary)
	require.Equal(t, "prod or staging?", asker.detail)

	reports, err := svc.ListReports(ctx, missionID, 10)
	require.NoError(t, err)
	require.Empty(t, reports, "a wired asker does not double-write a blocker report")
}

// TestUnit_MissionTools_AskAttentionFallsBackToBlocker pins that with no asker wired, the request lands as a durable blocker report.
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

// TestUnit_MissionTools_UnknownMissionSurfacesError pins that a report against a nonexistent mission surfaces not-found, not a silent insert.
func TestUnit_MissionTools_UnknownMissionSurfacesError(t *testing.T) {
	ctx, svc, _ := setup(t)
	tools := missiontools.New(svc, nil)

	_, _, err := tools.Exec(missiontools.WithMissionID(ctx, "no-such-mission"), time.Now(), nil, false, reportCall("progress", "ghost"))
	require.Error(t, err)
	require.ErrorIs(t, err, libdb.ErrNotFound)
}

// TestUnit_MissionTools_MetaRoundTrip pins that a mission id marshalled into `_meta` parses back out, and unrelated `_meta` does not.
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

// planModelCall builds a mission_plan call in the model-driven shape: entries as a []any under a map[string]any `input`.
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

// TestUnit_MissionTools_PlanSetsSnapshotAndEchoesIDs pins that mission_plan replaces the plan, stamps a heartbeat, and echoes assigned ids.
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

	echoed, ok := out.(missionservice.Plan)
	require.True(t, ok, "mission_plan echoes the stored Plan so ids carry forward")
	require.Equal(t, 1, echoed.Revision)
	require.Len(t, echoed.Entries, 2)
	require.NotEmpty(t, echoed.Entries[0].ID)
	require.NotEmpty(t, echoed.Entries[1].ID)
	require.Equal(t, "first cut", echoed.Explanation)

	m, err := svc.Get(ctx, missionID)
	require.NoError(t, err)
	require.Equal(t, 1, m.Plan.Revision)
	require.Len(t, m.Plan.Entries, 2)
	require.Equal(t, "survey the codebase", m.Plan.Entries[0].Content)
	require.Equal(t, missionservice.PlanEntryInProgress, m.Plan.Entries[0].Status)
	require.Equal(t, missionservice.PlanEntryPriorityHigh, m.Plan.Entries[0].Priority)
	require.NotNil(t, m.LastHeartbeat, "revising the plan is proof of life and heartbeats the mission")
}

// TestUnit_MissionTools_PlanAbsentOffMission pins that off a mission, mission_plan refuses to execute.
func TestUnit_MissionTools_PlanAbsentOffMission(t *testing.T) {
	ctx, svc, _ := setup(t)
	tools := missiontools.New(svc, nil)

	call, input := planModelCall("", planEntryArg("do a thing", "pending", "low"))
	_, _, err := tools.Exec(ctx, time.Now(), input, false, call)
	require.Error(t, err)
	require.Contains(t, err.Error(), "only available to a unit dispatched on a mission")
}

// TestUnit_MissionTools_PlanScopedToOwnMission pins that a unit plans only against the mission bound into its context.
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

// TestUnit_MissionTools_PlanReadsDeterministicJSONArgs pins that the deterministic Args path reaches entries as a JSON string.
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
	_, _, err := tools.Exec(missiontools.WithMissionID(ctx, missionID), time.Now(), nil, false, call)
	require.NoError(t, err)

	m, err := svc.Get(ctx, missionID)
	require.NoError(t, err)
	require.Len(t, m.Plan.Entries, 2)
	require.Equal(t, "step one", m.Plan.Entries[0].Content)
	require.Equal(t, missionservice.PlanEntryInProgress, m.Plan.Entries[0].Status)
	require.Equal(t, "seed from a deterministic chain", m.Plan.Explanation)
}

// TestUnit_MissionTools_PlanRejectsEmptySnapshot pins that an empty snapshot fails loudly rather than silently erasing the plan.
func TestUnit_MissionTools_PlanRejectsEmptySnapshot(t *testing.T) {
	ctx, svc, missionID := setup(t)
	tools := missiontools.New(svc, nil)

	call := &taskengine.ToolsCall{Name: missiontools.ToolsProviderName, ToolName: missiontools.ToolNamePlan}
	input := map[string]any{"entries": []any{}}
	_, _, err := tools.Exec(missiontools.WithMissionID(ctx, missionID), time.Now(), input, false, call)
	require.Error(t, err)
	require.Contains(t, err.Error(), "at least one entry")
}

// TestUnit_MissionTools_PlanRejectsUnknownStatus pins that a malformed enum surfaces through the store's validation, never coerced.
func TestUnit_MissionTools_PlanRejectsUnknownStatus(t *testing.T) {
	ctx, svc, missionID := setup(t)
	tools := missiontools.New(svc, nil)

	call, input := planModelCall("", planEntryArg("bad status", "halfway", "high"))
	_, _, err := tools.Exec(missiontools.WithMissionID(ctx, missionID), time.Now(), input, false, call)
	require.Error(t, err)
	require.Contains(t, err.Error(), "invalid plan entry status")
}

// TestUnit_MissionTools_PlanMissingEntriesRejected pins that a plan call with no entries argument is rejected, not a nil-plan write.
func TestUnit_MissionTools_PlanMissingEntriesRejected(t *testing.T) {
	ctx, svc, missionID := setup(t)
	tools := missiontools.New(svc, nil)

	call := &taskengine.ToolsCall{Name: missiontools.ToolsProviderName, ToolName: missiontools.ToolNamePlan}
	_, _, err := tools.Exec(missiontools.WithMissionID(ctx, missionID), time.Now(), map[string]any{}, false, call)
	require.Error(t, err)
	require.Contains(t, err.Error(), "requires an 'entries' array")
}

// TestUnit_MissionTools_PlanCarriesIDsForwardAndGuardsCompleted pins that a carried-forward id keeps identity, and completed content is immutable.
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

// TestUnit_MissionTools_FinishSetsTerminalStatus pins that mission_finish
// moves to the named terminal state and records the reason, and that the
// heartbeat every tool call ends with leaves a terminal mission at rest —
// liveness on a finished mission is meaningless, and stamping it would put an
// at-rest row back in motion past the abandoned-mission sweep.
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
	require.Nil(t, m.LastHeartbeat, "a terminal mission is left at rest, not heartbeated")
}

// TestUnit_MissionTools_FinishReadsModelStuck pins that the model-driven shape and a stuck verdict both work.
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

// TestUnit_MissionTools_FinishRequiresStatus pins that a finish call with no status is rejected before it reaches the store.
func TestUnit_MissionTools_FinishRequiresStatus(t *testing.T) {
	ctx, svc, missionID := setup(t)
	tools := missiontools.New(svc, nil)

	_, _, err := tools.Exec(missiontools.WithMissionID(ctx, missionID), time.Now(), nil, false, finishCall("", "no status"))
	require.Error(t, err)
	require.Contains(t, err.Error(), "requires a status")
}

// TestUnit_MissionTools_FinishRejectsNonTerminal pins that a non-terminal target (open) is refused by Finish's guard.
func TestUnit_MissionTools_FinishRejectsNonTerminal(t *testing.T) {
	ctx, svc, missionID := setup(t)
	tools := missiontools.New(svc, nil)

	_, _, err := tools.Exec(missiontools.WithMissionID(ctx, missionID), time.Now(), nil, false, finishCall("open", "not terminal"))
	require.Error(t, err)
	require.Contains(t, err.Error(), "terminal status is required")
}

// TestUnit_MissionTools_FinishIdempotentThenConflict pins that a same-status repeat is a no-op and a different-status repeat is a conflict.
func TestUnit_MissionTools_FinishIdempotentThenConflict(t *testing.T) {
	ctx, svc, missionID := setup(t)
	tools := missiontools.New(svc, nil)
	mctx := missiontools.WithMissionID(ctx, missionID)

	_, _, err := tools.Exec(mctx, time.Now(), nil, false, finishCall("landed", "done"))
	require.NoError(t, err)

	_, _, err = tools.Exec(mctx, time.Now(), nil, false, finishCall("landed", "done again"))
	require.NoError(t, err)

	_, _, err = tools.Exec(mctx, time.Now(), nil, false, finishCall("derailed", "reversal"))
	require.Error(t, err)
	require.Contains(t, err.Error(), "already finished")
}

// TestUnit_MissionTools_FinishAbsentOffMission pins that off a mission, mission_finish refuses to execute.
func TestUnit_MissionTools_FinishAbsentOffMission(t *testing.T) {
	ctx, svc, _ := setup(t)
	tools := missiontools.New(svc, nil)

	_, _, err := tools.Exec(ctx, time.Now(), nil, false, finishCall("landed", "should not run"))
	require.Error(t, err)
	require.Contains(t, err.Error(), "only available to a unit dispatched on a mission")
}

// TestUnit_MissionTools_AskAttentionFallsBackWhenUnanswered pins that an unanswered ask still lands as a durable, annotated blocker.
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

// marshalJSON renders a value as JSON for a schema-versus-descriptor comparison.
func marshalJSON(t *testing.T, v any) string {
	t.Helper()
	raw, err := json.Marshal(v)
	require.NoError(t, err)
	return string(raw)
}

// decodedSchema renders a descriptor's parameters or a published schema as
// plain JSON values, dropping an empty `properties` map: "declares no
// properties" and "declares an empty property set" are the same statement, and
// only the descriptor spells it out.
func decodedSchema(t *testing.T, v any) map[string]any {
	t.Helper()
	var out map[string]any
	require.NoError(t, json.Unmarshal([]byte(marshalJSON(t, v)), &out))
	if props, ok := out["properties"].(map[string]any); ok && len(props) == 0 {
		delete(out, "properties")
	}
	return out
}

// TestUnit_MissionTools_PublishedSchemaMatchesToolDescriptors pins the
// declared OpenAPI contract and its agreement with what actually reaches the
// model: every tool this provider declares — the unit's four and the
// supervisor's two — carries a request schema that IS its descriptor rendered,
// and a response schema describing what Exec returns for it.
func TestUnit_MissionTools_PublishedSchemaMatchesToolDescriptors(t *testing.T) {
	ctx, tools, _ := supervisorFixture(t)

	docs, err := tools.GetSchemasForSupportedTools(ctx)
	require.NoError(t, err)
	doc, ok := docs[missiontools.ToolsProviderName]
	require.True(t, ok, "the toolset publishes its contract under its provider name")
	require.Equal(t, "3.1.0", doc.OpenAPI)
	require.NotNil(t, doc.Info)
	require.NotEmpty(t, doc.Info.Title)
	require.NotEmpty(t, doc.Info.Description)
	require.NotEmpty(t, doc.Info.Version)
	require.NotNil(t, doc.Components)
	require.NoError(t, doc.Validate(ctx), "the published document is a valid OpenAPI document, not a shape that only looks like one")

	// The component name is part of the contract: a rename is a breaking change.
	components := map[string]string{
		missiontools.ToolNameReport:       "MissionReport",
		missiontools.ToolNameAskAttention: "MissionAskAttention",
		missiontools.ToolNamePlan:         "MissionPlan",
		missiontools.ToolNameFinish:       "MissionFinish",
		missiontools.ToolNameListMissions: "MissionList",
		missiontools.ToolNameAnswer:       "MissionAnswer",
	}
	require.Len(t, doc.Components.Schemas, 2*len(components)+1,
		"a request and a response per tool, plus the document mission_list returns as text")

	// Both gates, since neither alone lists everything the provider declares.
	unit, err := tools.GetToolsForToolsByName(missiontools.WithMissionID(ctx, "m-mine"), missiontools.ToolsProviderName)
	require.NoError(t, err)
	supervisor, err := tools.GetToolsForToolsByName(missiontools.WithParentSessionID(ctx, "cnx-parent"), missiontools.ToolsProviderName)
	require.NoError(t, err)
	declared := map[string]taskengine.Tool{}
	for _, tool := range append(unit, supervisor...) {
		declared[tool.Function.Name] = tool
	}
	require.Len(t, declared, len(components), "every tool the provider declares is published")

	for name, tool := range declared {
		component, ok := components[name]
		require.Truef(t, ok, "%s declares no OpenAPI component prefix", name)

		req := doc.Components.Schemas[component+"Request"]
		require.NotNilf(t, req, "%s: no request schema is published", name)
		require.Equalf(t, decodedSchema(t, tool.Function.Parameters), decodedSchema(t, req.Value),
			"%s: the published request must be the descriptor rendered, not a second copy of it", name)
		for prop, published := range req.Value.Properties {
			require.NotNilf(t, published.Value.Type, "%s.%s is typed", name, prop)
			require.NotEmptyf(t, published.Value.Description, "%s.%s is described", name, prop)
		}

		resp := doc.Components.Schemas[component+"Response"]
		require.NotNilf(t, resp, "%s: no response schema is published", name)
		require.NotEmptyf(t, resp.Value.Description+strings.Join(resp.Value.Required, ""),
			"%s: the response schema says nothing about what the tool returns", name)
	}

	// Closed value sets are declared as enums, not left to prose.
	report := doc.Components.Schemas["MissionReportRequest"]
	require.ElementsMatch(t, []string{"summary"}, report.Value.Required)
	require.ElementsMatch(t, []any{"progress", "finding", "blocker", "result"}, report.Value.Properties["kind"].Value.Enum)
	require.Contains(t, report.Value.Properties["kind"].Value.Description, "Omitted or blank files the report as progress.",
		"kind is enum'd but not required: the default execReport applies is part of the published contract")
	require.NotEmpty(t, report.Value.Properties["handover"].Value.Properties, "the nested hand-off shape survives publication")
	finish := doc.Components.Schemas["MissionFinishRequest"]
	require.ElementsMatch(t, []any{"landed", "derailed", "stuck", "abandoned"}, finish.Value.Properties["status"].Value.Enum)
	entries := doc.Components.Schemas["MissionPlanRequest"].Value.Properties["entries"]
	require.ElementsMatch(t, []string{"content", "status", "priority"}, entries.Value.Items.Value.Required,
		"the nested plan-entry shape survives publication")

	// Every tool but mission_plan answers with a line of text.
	for _, component := range []string{"MissionReport", "MissionAskAttention", "MissionFinish", "MissionList", "MissionAnswer"} {
		require.Truef(t, doc.Components.Schemas[component+"Response"].Value.Type.Is("string"),
			"%s returns text, and the published contract must say so", component)
	}
	require.NotNil(t, doc.Components.Schemas["MissionListPayload"], "the document mission_list serializes is declared too")

	// The plan response is declared against what execPlan actually returns.
	planSchema := doc.Components.Schemas["MissionPlanResponse"]
	require.True(t, planSchema.Value.Type.Is("object"))
	planCtx, svc, missionID := setup(t)
	out, dt, err := missiontools.New(svc, nil).Exec(missiontools.WithMissionID(planCtx, missionID), time.Now(), map[string]any{
		"entries":     []any{map[string]any{"content": "read the code", "status": "in_progress", "priority": "high"}},
		"explanation": "first cut",
	}, false, &taskengine.ToolsCall{Name: missiontools.ToolsProviderName, ToolName: missiontools.ToolNamePlan})
	require.NoError(t, err)
	require.Equal(t, taskengine.DataTypeJSON, dt)
	var got map[string]any
	require.NoError(t, json.Unmarshal([]byte(marshalJSON(t, out)), &got))
	for key := range got {
		require.Containsf(t, planSchema.Value.Properties, key, "the plan payload carries %s but the published schema does not declare it", key)
	}
	for _, key := range planSchema.Value.Required {
		require.Containsf(t, got, key, "the published schema requires %s but the payload omits it", key)
	}
	entry := got["entries"].([]any)[0].(map[string]any)
	for key := range entry {
		require.Containsf(t, planSchema.Value.Properties["entries"].Value.Items.Value.Properties, key,
			"a stored plan entry carries %s but the published schema does not declare it", key)
	}
}

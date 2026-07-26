package missionservice

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// ─── typed handover on reports ──────────────────────────────────────────────

// A report may carry a full typed hand-off; AddReport stores it verbatim and
// ListReports round-trips it, so the next mission reads real context rather than
// prose off the report.
func TestUnit_AddReport_StoresTypedHandover(t *testing.T) {
	ctx, db := setupMissionDB(t)
	svc := New(db)

	m := newMission("port the hot loop")
	require.NoError(t, svc.Create(ctx, m))

	want := &Handover{
		Outcome:         "ported the hot loop to Rust; benchmarks pending",
		Artifacts:       []string{"src/hotloop.rs", "https://ci/run/42"},
		HandoverForNext: "pick up the benchmark harness; the port compiles and passes unit tests",
		Caveats:         "SIMD path is untested on aarch64",
	}
	require.NoError(t, svc.AddReport(ctx, m.ID, &Report{
		Kind:     ReportKindResult,
		Summary:  "hot loop ported",
		Handover: want,
	}))

	reports, err := svc.ListReports(ctx, m.ID, 10)
	require.NoError(t, err)
	require.Len(t, reports, 1)
	require.NotNil(t, reports[0].Handover, "a filed hand-off survives storage")
	require.Equal(t, want.Outcome, reports[0].Handover.Outcome)
	require.Equal(t, want.Artifacts, reports[0].Handover.Artifacts)
	require.Equal(t, want.HandoverForNext, reports[0].Handover.HandoverForNext)
	require.Equal(t, want.Caveats, reports[0].Handover.Caveats)
}

// A report without a hand-off is a legacy report: it stores and reads back with
// a nil Handover, fully compatible with every report written before the field
// existed.
func TestUnit_AddReport_AbsentHandoverIsLegacyReport(t *testing.T) {
	ctx, db := setupMissionDB(t)
	svc := New(db)

	m := newMission("no hand-off here")
	require.NoError(t, svc.Create(ctx, m))
	require.NoError(t, svc.AddReport(ctx, m.ID, &Report{Kind: ReportKindProgress, Summary: "halfway"}))

	reports, err := svc.ListReports(ctx, m.ID, 10)
	require.NoError(t, err)
	require.Len(t, reports, 1)
	require.Nil(t, reports[0].Handover, "a report with no hand-off carries a nil Handover")
}

// An all-empty hand-off is collapsed to nil at storage, so "a hand-off with
// nothing in it" and "no hand-off" are the same durable fact.
func TestUnit_AddReport_EmptyHandoverCollapsesToNil(t *testing.T) {
	ctx, db := setupMissionDB(t)
	svc := New(db)

	m := newMission("empty hand-off")
	require.NoError(t, svc.Create(ctx, m))
	require.NoError(t, svc.AddReport(ctx, m.ID, &Report{
		Kind:     ReportKindFinding,
		Summary:  "found nothing worth handing over",
		Handover: &Handover{Outcome: "  ", Artifacts: []string{"", "   "}},
	}))

	reports, err := svc.ListReports(ctx, m.ID, 10)
	require.NoError(t, err)
	require.Len(t, reports, 1)
	require.Nil(t, reports[0].Handover, "an all-blank hand-off is stored as no hand-off")
}

// The hand-off rides the ReportAddedEvent on the bus, so a routing service that
// forwards the report to the next mission has the full hand-off in the event
// (the self-contained-payload rule).
func TestUnit_AddReport_HandoverRidesTheEvent(t *testing.T) {
	ctx, db := setupMissionDB(t)
	pub := &fakePublisher{}
	svc := New(db, WithEventPublisher(pub))

	m := newMission("supervised hand-off")
	m.ParentSessionID = "parent-9"
	require.NoError(t, svc.Create(ctx, m))
	require.NoError(t, svc.AddReport(ctx, m.ID, &Report{
		Kind:     ReportKindResult,
		Summary:  "shipped",
		Handover: &Handover{Outcome: "shipped the board", HandoverForNext: "wire the inbox next"},
	}))

	evs := pub.events(t)
	require.Len(t, evs, 1)
	require.NotNil(t, evs[0].Report.Handover, "the hand-off rides the event without a read-back")
	require.Equal(t, "shipped the board", evs[0].Report.Handover.Outcome)
	require.Equal(t, "wire the inbox next", evs[0].Report.Handover.HandoverForNext)
}

// ─── validateHandover shape matrix ──────────────────────────────────────────

func TestUnit_ValidateHandover(t *testing.T) {
	tests := []struct {
		name     string
		handover *Handover
		wantErr  bool
	}{
		{name: "nil hand-off is valid", handover: nil},
		{name: "a full, reasonable hand-off", handover: &Handover{
			Outcome:         "done",
			Artifacts:       []string{"a.rs", "b.rs"},
			HandoverForNext: "keep going",
			Caveats:         "watch the edge case",
		}},
		{name: "oversized outcome is rejected", handover: &Handover{Outcome: strings.Repeat("x", maxHandoverTextBytes+1)}, wantErr: true},
		{name: "oversized handoverForNext is rejected", handover: &Handover{HandoverForNext: strings.Repeat("y", maxHandoverTextBytes+1)}, wantErr: true},
		{name: "oversized caveats is rejected", handover: &Handover{Caveats: strings.Repeat("z", maxHandoverTextBytes+1)}, wantErr: true},
		{name: "stream-leak in a field is rejected", handover: &Handover{Outcome: "self.__next_f.push([1, \"chunk\"])"}, wantErr: true},
		{name: "too many artifacts is rejected", handover: &Handover{Artifacts: manyArtifacts(maxHandoverArtifacts + 1)}, wantErr: true},
		{name: "artifacts at the count cap are accepted", handover: &Handover{Artifacts: manyArtifacts(maxHandoverArtifacts)}},
		{name: "oversized artifact ref is rejected", handover: &Handover{Artifacts: []string{strings.Repeat("p", maxHandoverArtifactBytes+1)}}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateHandover(tt.handover)
			if tt.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func manyArtifacts(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = "artifact"
	}
	return out
}

// A report whose hand-off violates a shape cap is rejected at AddReport, so the
// tool that filed it gets a legible error to correct — the report is not stored
// with a corrupt hand-off.
func TestUnit_AddReport_RejectsOversizedHandover(t *testing.T) {
	ctx, db := setupMissionDB(t)
	svc := New(db)

	m := newMission("guard the hand-off")
	require.NoError(t, svc.Create(ctx, m))
	err := svc.AddReport(ctx, m.ID, &Report{
		Kind:     ReportKindResult,
		Summary:  "done",
		Handover: &Handover{Outcome: strings.Repeat("x", maxHandoverTextBytes+1)},
	})
	require.Error(t, err)

	reports, lerr := svc.ListReports(ctx, m.ID, 10)
	require.NoError(t, lerr)
	require.Empty(t, reports, "a report with a corrupt hand-off is not stored")
}

// The Report JSON is additive: a hand-off-carrying report and a legacy report
// differ only by the presence of the `handover` key, and a legacy JSON blob (no
// key) decodes to a nil Handover.
func TestUnit_Report_HandoverJSONIsAdditive(t *testing.T) {
	var legacy Report
	require.NoError(t, json.Unmarshal([]byte(`{"id":"r1","kind":"result","summary":"done"}`), &legacy))
	require.Nil(t, legacy.Handover, "a legacy report JSON with no handover key decodes to nil")

	withHandover := Report{ID: "r2", Kind: ReportKindResult, Summary: "done", Handover: &Handover{Outcome: "shipped"}}
	raw, err := json.Marshal(withHandover)
	require.NoError(t, err)
	require.Contains(t, string(raw), `"handover"`)

	legacyRaw, err := json.Marshal(legacy)
	require.NoError(t, err)
	require.NotContains(t, string(legacyRaw), `"handover"`, "a nil hand-off is omitted from the wire")
}

package acpsvc

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/contenox/beam/internal/kernel/taskengine"
	libdb "github.com/contenox/beam/internal/libdbexec"
	"github.com/contenox/beam/internal/services/missionservice"
	"github.com/contenox/beam/internal/services/missiontools"
	"github.com/contenox/beam/internal/store/runtimetypes"
	"github.com/contenox/beam/libacp"
	"github.com/stretchr/testify/require"
)

// planSnapshotJSON marshals a missionservice.Plan the way taskengine serializes a
// DataTypeJSON tool result into a task event's Content (serializeToolResultContent
// == json.Marshal for JSON results). Using the real type here means the
// projection is tested against the EXACT bytes the mission_plan tool emits, not a
// hand-written stand-in that could drift from it.
func planSnapshotJSON(t *testing.T, plan missionservice.Plan) string {
	t.Helper()
	raw, err := json.Marshal(plan)
	require.NoError(t, err)
	return string(raw)
}

// TestUnit_PlanProjection_FullSnapshotCast is the core of the projection: a
// successful mission_plan tool event becomes a full-snapshot `plan` session
// update whose entries are cast 1:1 onto libacp.PlanEntry — every entry, in
// order, with content/status/priority carried across.
func TestUnit_PlanProjection_FullSnapshotCast(t *testing.T) {
	plan := missionservice.Plan{
		Revision: 2,
		Entries: []missionservice.PlanEntry{
			{ID: "a", Content: "survey the codebase", Status: missionservice.PlanEntryCompleted, Priority: missionservice.PlanEntryPriorityHigh},
			{ID: "b", Content: "port the hot loop", Status: missionservice.PlanEntryInProgress, Priority: missionservice.PlanEntryPriorityMedium},
			{ID: "c", Content: "benchmark", Status: missionservice.PlanEntryPending, Priority: missionservice.PlanEntryPriorityLow},
		},
	}
	ev := taskengine.TaskEvent{
		Kind:     taskengine.TaskEventToolCall,
		ToolName: planToolEventName,
		Content:  planSnapshotJSON(t, plan),
	}

	note, ok := planUpdateNotification(libacp.SessionID("sess-1"), ev)
	require.True(t, ok)
	require.Equal(t, libacp.SessionID("sess-1"), note.SessionID)
	require.Equal(t, libacp.SessionUpdatePlan, note.Update.SessionUpdate)
	require.Len(t, note.Update.Entries, 3, "the projection is a FULL snapshot — the entire entries list")

	require.Equal(t, libacp.PlanEntry{Content: "survey the codebase", Status: libacp.PlanStatusCompleted, Priority: libacp.PlanPriorityHigh}, note.Update.Entries[0])
	require.Equal(t, libacp.PlanEntry{Content: "port the hot loop", Status: libacp.PlanStatusInProgress, Priority: libacp.PlanPriorityMedium}, note.Update.Entries[1])
	require.Equal(t, libacp.PlanEntry{Content: "benchmark", Status: libacp.PlanStatusPending, Priority: libacp.PlanPriorityLow}, note.Update.Entries[2])

	// The plan update serializes under the `entries` wire key per ACP semantics.
	wire, err := json.Marshal(note.Update)
	require.NoError(t, err)
	var generic map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(wire, &generic))
	require.Equal(t, `"plan"`, string(generic["sessionUpdate"]))
	require.Contains(t, generic, "entries")
}

// TestUnit_PlanProjection_IgnoresNonPlanAndFailedEvents proves the projection
// no-ops for everything that is not a successful mission_plan snapshot, so it
// never emits a spurious or corrupt `plan` update.
func TestUnit_PlanProjection_IgnoresNonPlanAndFailedEvents(t *testing.T) {
	goodContent := planSnapshotJSON(t, missionservice.Plan{
		Revision: 1,
		Entries:  []missionservice.PlanEntry{{ID: "a", Content: "do", Status: missionservice.PlanEntryPending, Priority: missionservice.PlanEntryPriorityHigh}},
	})

	cases := []struct {
		name string
		ev   taskengine.TaskEvent
	}{
		{"a different tool", taskengine.TaskEvent{Kind: taskengine.TaskEventToolCall, ToolName: "mission.mission_report", Content: goodContent}},
		{"the report tool", taskengine.TaskEvent{Kind: taskengine.TaskEventToolCall, ToolName: "local_fs.write_file", Content: goodContent}},
		{"a plan call that errored", taskengine.TaskEvent{Kind: taskengine.TaskEventToolCall, ToolName: planToolEventName, Content: goodContent, Error: "set plan: boom"}},
		{"unparseable content", taskengine.TaskEvent{Kind: taskengine.TaskEventToolCall, ToolName: planToolEventName, Content: "not json {"}},
		{"empty content", taskengine.TaskEvent{Kind: taskengine.TaskEventToolCall, ToolName: planToolEventName, Content: ""}},
		{"parses but has no entries", taskengine.TaskEvent{Kind: taskengine.TaskEventToolCall, ToolName: planToolEventName, Content: `{"revision":3,"entries":[]}`}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, ok := planUpdateNotification(libacp.SessionID("sess-1"), c.ev)
			require.False(t, ok, "no plan update should be emitted")
		})
	}
}

// TestUnit_PlanProjection_EnumParity is the parity test Slice 1's missionservice
// comments promise lives in the projection slice: the plan status/priority enums
// are contracted byte-for-byte equal between missionservice (the durable record)
// and libacp (the wire), which is the whole reason the projection can cast rather
// than translate. If either set ever drifts, this goes red before a silently
// mistranslated plan reaches an editor.
func TestUnit_PlanProjection_EnumParity(t *testing.T) {
	require.Equal(t, string(libacp.PlanStatusPending), string(missionservice.PlanEntryPending))
	require.Equal(t, string(libacp.PlanStatusInProgress), string(missionservice.PlanEntryInProgress))
	require.Equal(t, string(libacp.PlanStatusCompleted), string(missionservice.PlanEntryCompleted))

	require.Equal(t, string(libacp.PlanPriorityHigh), string(missionservice.PlanEntryPriorityHigh))
	require.Equal(t, string(libacp.PlanPriorityMedium), string(missionservice.PlanEntryPriorityMedium))
	require.Equal(t, string(libacp.PlanPriorityLow), string(missionservice.PlanEntryPriorityLow))
}

// TestUnit_PlanProjection_ReachesAttachedViewer is the composed acceptance for
// the projection: the REAL mission_plan tool writes a plan through the REAL
// mission service, its echoed snapshot rides a tool event through the REAL
// transport's event translation, and a REAL ACP client attached over the
// loopback wire receives a full-snapshot `plan` session update matching the
// persisted plan. It is hermetic and fast (in-process pipes + sqlite, no
// subprocess), so it runs in the TestUnit_ gate.
//
// Why this composed shape rather than a subprocess e2e (the e2e_mission_report
// idiom named in the brief): the projection seam is the transport's event
// translation, and what it must prove is "a mission_plan call's snapshot reaches
// an attached viewer as a `plan` update, and the plan is persisted." A dispatched
// `contenox acp` subprocess would additionally need a viewer ATTACHED to the
// unit's own session to observe its stream — machinery no existing e2e stands up
// (e2e_mission_report reads the durable store, never attaches to the unit) — for
// no added coverage of THIS seam: the subprocess boundary is already proven by
// the mission-tools e2e, and the projection is pure transport-side translation.
// This test exercises every real component the projection touches (tool → store →
// engine-faithful JSON serialization → transport translation → wire → client),
// stubbing only the subprocess the seam does not depend on.
func TestUnit_PlanProjection_ReachesAttachedViewer(t *testing.T) {
	ctx := context.Background()

	// The real mission store, and a mission to plan against.
	db, err := libdb.NewSQLiteDBManager(ctx, filepath.Join(t.TempDir(), "plan-projection.db"), runtimetypes.SchemaSQLite)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	missions := missionservice.New(db)
	mission := &missionservice.Mission{Intent: "migrate the loop", AgentName: "planner", HITLPolicyName: "default"}
	require.NoError(t, missions.Create(ctx, mission))

	// The real mission_plan tool writes a full snapshot and echoes it back — the
	// exact result the engine turns into the tool event's Content.
	tools := missiontools.New(missions, nil)
	planCall := &taskengine.ToolsCall{
		Name:     missiontools.ToolsProviderName,
		ToolName: missiontools.ToolNamePlan,
		Args: map[string]string{
			"entries":     `[{"content":"survey the codebase","status":"in_progress","priority":"high"},{"content":"port the hot loop","status":"pending","priority":"medium"}]`,
			"explanation": "first cut",
		},
	}
	result, resultType, err := tools.Exec(missiontools.WithMissionID(ctx, mission.ID), time.Now(), nil, false, planCall)
	require.NoError(t, err)
	require.Equal(t, taskengine.DataTypeJSON, resultType)

	// The plan is persisted on the mission (the durable half of the acceptance).
	stored, err := missions.Get(ctx, mission.ID)
	require.NoError(t, err)
	require.Equal(t, 1, stored.Plan.Revision)
	require.Len(t, stored.Plan.Entries, 2)

	// Build the tool event the engine would publish from that result, serialized
	// exactly as taskengine serializes a DataTypeJSON result.
	echoed := result.(missionservice.Plan)
	ev := taskengine.TaskEvent{
		Kind:       taskengine.TaskEventToolCall,
		ToolName:   planToolEventName,
		ApprovalID: "plan-call-1",
		Content:    planSnapshotJSON(t, echoed),
	}
	payload, err := json.Marshal(ev)
	require.NoError(t, err)

	// Drive it through the REAL transport's translation to a REAL attached client.
	h := newLoopbackHarness(t)
	sid := libacp.SessionID("unit-session")
	h.tr.publishEvent(ctx, sid, payload)

	// The plan event yields two updates on the wire: the tool-call card and the
	// plan snapshot. Find the plan update and assert its entries.
	var planNote *libacp.SessionNotification
	for _, n := range h.lc.drain(t, 2) {
		if n.Update.SessionUpdate == libacp.SessionUpdatePlan {
			nn := n
			planNote = &nn
			break
		}
	}
	require.NotNil(t, planNote, "the attached viewer must receive a `plan` session update")
	require.Equal(t, sid, planNote.SessionID, "the projection is emitted on the OWNING unit's session")
	require.Len(t, planNote.Update.Entries, 2)
	require.Equal(t, libacp.PlanEntry{Content: "survey the codebase", Status: libacp.PlanStatusInProgress, Priority: libacp.PlanPriorityHigh}, planNote.Update.Entries[0])
	require.Equal(t, libacp.PlanEntry{Content: "port the hot loop", Status: libacp.PlanStatusPending, Priority: libacp.PlanPriorityMedium}, planNote.Update.Entries[1])
}

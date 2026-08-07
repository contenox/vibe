package fleetservice

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/contenox/contenox/internal/kernel/agentinstance"
	"github.com/contenox/contenox/internal/kernel/taskengine"
	"github.com/contenox/contenox/internal/services/hitlservice"
	"github.com/contenox/contenox/internal/services/missionservice"
	"github.com/contenox/contenox/internal/store/runtimetypes"
	"github.com/contenox/contenox/libacp"
	libdb "github.com/contenox/contenox/libdbexec"
	"github.com/contenox/contenox/libtracker"
	"github.com/stretchr/testify/require"
)

// Tests the compute bounds enforcement seams in isolation: the pure
// predicates and counter, the unattended answerer's maxToolCalls refusal,
// and the drive loop's maxTurns/maxTokens stuck landing. Subprocess
// acceptance is e2e_compute_bounds_test.go.

// ─── test doubles ───────────────────────────────────────────────────────────

// fakeBoundsReader returns a fixed compute ceiling for any policy name.
type fakeBoundsReader struct {
	bounds hitlservice.ComputeBounds
	err    error
}

func (f fakeBoundsReader) ComputeBoundsFor(context.Context, string) (hitlservice.ComputeBounds, error) {
	return f.bounds, f.err
}

// boundedHITL is a fakeHITL (see unattended_test.go) that also implements
// ComputeBoundsReader, the shape the answerer type-asserts for maxToolCalls.
type boundedHITL struct {
	*fakeHITL
	bounds hitlservice.ComputeBounds
}

func (b boundedHITL) ComputeBoundsFor(context.Context, string) (hitlservice.ComputeBounds, error) {
	return b.bounds, nil
}

func computeTestDB(t *testing.T) (context.Context, libdb.DBManager) {
	t.Helper()
	ctx := context.Background()
	db, err := libdb.NewSQLiteDBManager(ctx, filepath.Join(t.TempDir(), "compute.db"), runtimetypes.SchemaSQLite)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	return ctx, db
}

func usageNote(used int) libacp.SessionNotification {
	return libacp.SessionNotification{
		SessionID: "sess",
		Update:    libacp.SessionUpdate{SessionUpdate: libacp.SessionUpdateUsageUpdate, Used: used},
	}
}

// ─── pure predicates + counter ──────────────────────────────────────────────

func TestUnit_Compute_TurnBudgetExceeded(t *testing.T) {
	t.Parallel()
	unbounded := hitlservice.ComputeBounds{}      // MaxTurns 0
	one := hitlservice.ComputeBounds{MaxTurns: 1} // only the intent turn
	require.False(t, turnBudgetExceeded(1, unbounded), "an absent bound never exceeds — today's behavior")
	require.False(t, turnBudgetExceeded(99, unbounded))
	require.False(t, turnBudgetExceeded(1, one), "the intent turn is within a budget of 1")
	require.True(t, turnBudgetExceeded(2, one), "the nudge (turn 2) exceeds a budget of 1")
}

func TestUnit_Compute_ToolCallBudgetExceeded(t *testing.T) {
	t.Parallel()
	unbounded := hitlservice.ComputeBounds{}
	one := hitlservice.ComputeBounds{MaxToolCalls: 1}
	require.False(t, toolCallBudgetExceeded(1000, unbounded), "an absent bound never exceeds")
	require.False(t, toolCallBudgetExceeded(1, one), "the first gated call is within a budget of 1")
	require.True(t, toolCallBudgetExceeded(2, one), "the second gated call crosses a budget of 1")
}

func TestUnit_Compute_TokenBudgetExceeded(t *testing.T) {
	t.Parallel()
	require.False(t, tokenBudgetExceeded(9_000_000, hitlservice.ComputeBounds{}), "an absent bound never exceeds")
	require.False(t, tokenBudgetExceeded(100, hitlservice.ComputeBounds{MaxTokens: 100}), "at the ceiling is not over it")
	require.True(t, tokenBudgetExceeded(101, hitlservice.ComputeBounds{MaxTokens: 100}))
}

// TestUnit_Compute_ReasonsNameTheBound: every exhaustion reason carries the
// stable lead and names the bound that was crossed. pause_ask no longer
// exists (rejected at policy validation), so no reason ever appends a
// deferral note.
func TestUnit_Compute_ReasonsNameTheBound(t *testing.T) {
	t.Parallel()
	for _, b := range []hitlservice.ComputeBounds{
		{MaxTurns: 1, MaxToolCalls: 2, MaxTokens: 3},
		{MaxTurns: 1, MaxToolCalls: 2, MaxTokens: 3, OnExhausted: hitlservice.OnExhaustedFinishStuck},
	} {
		require.Contains(t, turnsExhaustedReason(b), computeBoundLead)
		require.Contains(t, turnsExhaustedReason(b), "maxTurns=1")
		require.Contains(t, toolCallsExhaustedReason(b), "maxToolCalls=2")
		require.Contains(t, tokensExhaustedReason(b, 99), "maxTokens=3")
		require.Contains(t, tokensExhaustedReason(b, 99), "99")
		require.NotContains(t, turnsExhaustedReason(b), "NOT IMPLEMENTED")
	}
}

func TestUnit_Compute_JournalTokenUsage(t *testing.T) {
	t.Parallel()

	used, present := journalTokenUsage(nil)
	require.False(t, present, "no usage_update means no reported usage — maxTokens stays inert")
	require.Zero(t, used)

	// A non-usage update contributes nothing.
	notes := []libacp.SessionNotification{
		{SessionID: "s", Update: libacp.SessionUpdate{SessionUpdate: libacp.SessionUpdateAgentMessageChunk}},
	}
	_, present = journalTokenUsage(notes)
	require.False(t, present)

	// The max Used across usage_updates is the latest figure regardless of order.
	notes = []libacp.SessionNotification{usageNote(120), usageNote(4000), usageNote(900)}
	used, present = journalTokenUsage(notes)
	require.True(t, present)
	require.Equal(t, 4000, used)
}

func TestUnit_Compute_MissionCallCounter_CountsPerMission(t *testing.T) {
	t.Parallel()
	c := newMissionCallCounter(0)
	require.Equal(t, 1, c.increment("m1"))
	require.Equal(t, 2, c.increment("m1"))
	require.Equal(t, 1, c.increment("m2"), "each mission counts independently")
	require.Equal(t, 3, c.increment("m1"))
}

func TestUnit_Compute_MissionCallCounter_EvictsOldestWhenFull(t *testing.T) {
	t.Parallel()
	c := newMissionCallCounter(2)
	c.increment("m1")
	c.increment("m2")
	c.increment("m3") // evicts m1 (oldest)
	require.Len(t, c.counts, 2, "the tally stays bounded")
	require.Equal(t, 1, c.increment("m1"), "an evicted mission restarts its count — a benign degradation")
}

// ─── the answerer: maxToolCalls refuses the crossing call + finishes stuck ──

func TestUnit_Compute_Answerer_RefusesCallOverBudgetAndFinishesStuck(t *testing.T) {
	ctx, db := computeTestDB(t)
	missions := missionservice.New(db)

	m := &missionservice.Mission{ID: "mission-tc", Intent: "do work", AgentName: "reviewer", HITLPolicyName: "envelope.json"}
	require.NoError(t, missions.Create(ctx, m))
	_, err := missions.Bind(ctx, m.ID, "sess-1", "inst-1")
	require.NoError(t, err)

	hitl := boundedHITL{
		fakeHITL: &fakeHITL{verdict: hitlservice.EvaluationResult{Action: hitlservice.ActionAllow}},
		bounds:   hitlservice.ComputeBounds{MaxToolCalls: 1},
	}
	answer := NewUnattendedPermissionAnswerer(UnattendedPermissionDeps{
		HITL:     hitl,
		Missions: missions,
		Sink:     taskengine.NoopTaskEventSink{},
		Tracker:  libtracker.NoopTracker{},
	})

	req := namedRequest(t, "local_fs", "write_file", map[string]any{"path": "/x"})

	// Call 1 is within budget: allowed.
	resp1, err := answer(ctx, unattended(req))
	require.NoError(t, err)
	require.Equal(t, "yes", resp1.Outcome.OptionID, "the first gated call is inside the budget and is allowed")
	m1, err := missions.Get(ctx, m.ID)
	require.NoError(t, err)
	require.Equal(t, missionservice.StatusOpen, m1.Status, "one gated call must not finish the mission")

	// Call 2 crosses the budget: refused, mission lands stuck.
	resp2, err := answer(ctx, unattended(req))
	require.NoError(t, err)
	require.Equal(t, "no", resp2.Outcome.OptionID, "the call that crosses maxToolCalls gets the teaching refusal")

	m2, err := missions.Get(ctx, m.ID)
	require.NoError(t, err)
	require.Equal(t, missionservice.StatusStuck, m2.Status, "crossing maxToolCalls lands the mission stuck")
	require.Contains(t, m2.StatusReason, "maxToolCalls=1", "the terminal reason names the bound it crossed")
	require.Contains(t, m2.StatusReason, computeBoundLead)
}

// TestUnit_Compute_Answerer_NoBoundNeverRefuses: no maxToolCalls block means
// never counted, never refused.
func TestUnit_Compute_Answerer_NoBoundNeverRefuses(t *testing.T) {
	ctx, db := computeTestDB(t)
	missions := missionservice.New(db)
	m := &missionservice.Mission{ID: "mission-unb", Intent: "do work", AgentName: "reviewer", HITLPolicyName: "envelope.json"}
	require.NoError(t, missions.Create(ctx, m))
	_, err := missions.Bind(ctx, m.ID, "sess-1", "inst-1")
	require.NoError(t, err)

	hitl := boundedHITL{
		fakeHITL: &fakeHITL{verdict: hitlservice.EvaluationResult{Action: hitlservice.ActionAllow}},
		bounds:   hitlservice.ComputeBounds{}, // no maxToolCalls
	}
	answer := NewUnattendedPermissionAnswerer(UnattendedPermissionDeps{
		HITL: hitl, Missions: missions, Sink: taskengine.NoopTaskEventSink{}, Tracker: libtracker.NoopTracker{},
	})
	req := namedRequest(t, "local_fs", "read_file", map[string]any{"path": "/x"})
	for i := 0; i < 5; i++ {
		resp, err := answer(ctx, unattended(req))
		require.NoError(t, err)
		require.Equal(t, "yes", resp.Outcome.OptionID)
	}
	got, err := missions.Get(ctx, m.ID)
	require.NoError(t, err)
	require.Equal(t, missionservice.StatusOpen, got.Status, "an unbounded mission is never finished by the answerer")
}

// ─── the drive loop: maxTurns + maxTokens land the mission stuck ────────────

// journalManager is a fakeManager that also serves a session journal, for
// the drive loop's maxTokens read.
type journalManager struct {
	*fakeManager
	journal []libacp.SessionNotification
}

func (m *journalManager) SessionJournal(string, libacp.SessionID) ([]libacp.SessionNotification, string, bool) {
	return m.journal, "", true
}

func driveFixture(t *testing.T, bounds hitlservice.ComputeBounds, mgr agentinstance.Manager) (*service, missionservice.Service, missionRun) {
	t.Helper()
	ctx, db := computeTestDB(t)
	missions := missionservice.New(db)
	m := &missionservice.Mission{ID: "mission-drive", Intent: "run the mission", AgentName: "unit", HITLPolicyName: "envelope.json"}
	require.NoError(t, missions.Create(ctx, m))
	_, err := missions.Bind(ctx, m.ID, "sess-1", "inst-1")
	require.NoError(t, err)

	svc := New(mgr, nil, missions, nil, t.TempDir(), libtracker.NoopTracker{},
		WithComputeBounds(fakeBoundsReader{bounds: bounds})).(*service)
	run := missionRun{instanceID: "inst-1", sessionID: "sess-1", missionID: m.ID, agentName: "unit", intent: m.Intent}
	return svc, missions, run
}

func TestUnit_Compute_DriveLoop_MaxTurnsLandsStuckBeforeNudge(t *testing.T) {
	mgr := &fakeManager{openID: "sess-1"}
	svc, missions, run := driveFixture(t, hitlservice.ComputeBounds{MaxTurns: 1}, mgr)

	svc.driveUnattendedMission(context.Background(), run)

	require.Equal(t, 1, mgr.promptCalls, "maxTurns=1 must stop before the nudge: exactly one prompt turn")
	got, err := missions.Get(context.Background(), run.missionID)
	require.NoError(t, err)
	require.Equal(t, missionservice.StatusStuck, got.Status, "a mission out of its turn budget lands stuck")
	require.Contains(t, got.StatusReason, "maxTurns=1")
	require.NotNil(t, got.LastHeartbeat, "turn completion still stamps liveness")
}

func TestUnit_Compute_DriveLoop_MaxTokensLandsStuck(t *testing.T) {
	mgr := &journalManager{fakeManager: &fakeManager{openID: "sess-1"}, journal: []libacp.SessionNotification{usageNote(500)}}
	svc, missions, run := driveFixture(t, hitlservice.ComputeBounds{MaxTokens: 100}, mgr)

	svc.driveUnattendedMission(context.Background(), run)

	require.Equal(t, 1, mgr.promptCalls, "the token budget is spent on turn 1; no nudge follows")
	got, err := missions.Get(context.Background(), run.missionID)
	require.NoError(t, err)
	require.Equal(t, missionservice.StatusStuck, got.Status)
	require.Contains(t, got.StatusReason, "maxTokens=100")
	require.Contains(t, got.StatusReason, "500", "the reason quotes the reported usage that crossed the bound")
}

// TestUnit_Compute_DriveLoop_TokenBudgetNotExceededKeepsNormalPath: usage
// under budget leaves the mission on its normal mute-unit path.
func TestUnit_Compute_DriveLoop_TokenBudgetNotExceededKeepsNormalPath(t *testing.T) {
	mgr := &journalManager{fakeManager: &fakeManager{openID: "sess-1", agentText: "still working"}, journal: []libacp.SessionNotification{usageNote(50)}}
	svc, missions, run := driveFixture(t, hitlservice.ComputeBounds{MaxTokens: 1_000_000}, mgr)

	svc.driveUnattendedMission(context.Background(), run)

	require.Equal(t, 2, mgr.promptCalls, "within budget, the mute-unit path still runs its one nudge")
	got, err := missions.Get(context.Background(), run.missionID)
	require.NoError(t, err)
	require.Equal(t, missionservice.StatusOpen, got.Status, "a within-budget mute unit is blocked, not compute-stuck")
	reps, err := missions.ListReports(context.Background(), run.missionID, 5)
	require.NoError(t, err)
	require.Len(t, reps, 1)
	require.Equal(t, missionservice.ReportKindBlocker, reps[0].Kind)
}

func TestUnit_Compute_DriveLoop_NoReaderIsUnbounded(t *testing.T) {
	ctx, db := computeTestDB(t)
	missions := missionservice.New(db)
	m := &missionservice.Mission{ID: "mission-noreader", Intent: "run", AgentName: "unit", HITLPolicyName: "envelope.json"}
	require.NoError(t, missions.Create(ctx, m))
	_, err := missions.Bind(ctx, m.ID, "sess-1", "inst-1")
	require.NoError(t, err)

	mgr := &fakeManager{openID: "sess-1", agentText: "hello"}
	// No WithComputeBounds: every mission is unbounded.
	svc := New(mgr, nil, missions, nil, t.TempDir(), libtracker.NoopTracker{}).(*service)
	run := missionRun{instanceID: "inst-1", sessionID: "sess-1", missionID: m.ID, agentName: "unit", intent: m.Intent}

	svc.driveUnattendedMission(ctx, run)

	require.Equal(t, 2, mgr.promptCalls, "with no bounds reader, the mission runs its full intent+nudge path")
	got, err := missions.Get(ctx, m.ID)
	require.NoError(t, err)
	require.Equal(t, missionservice.StatusOpen, got.Status, "unbounded: no compute stuck")
}

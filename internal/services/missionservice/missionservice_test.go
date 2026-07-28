package missionservice

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/contenox/beam/internal/errdefs"
	libdb "github.com/contenox/beam/internal/libdbexec"
	"github.com/contenox/beam/internal/store/runtimetypes"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func setupMissionDB(t *testing.T) (context.Context, libdb.DBManager) {
	t.Helper()
	ctx := context.Background()
	db, err := libdb.NewSQLiteDBManager(ctx, filepath.Join(t.TempDir(), "missionservice.db"), runtimetypes.SchemaSQLite)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	return ctx, db
}

const testPolicy = "hitl-policy-default.json"

func newMission(intent string) *Mission {
	return &Mission{Intent: intent, AgentName: "runner", HITLPolicyName: testPolicy}
}

// ─── validate() table test ─────────────────────────────────────────────────

func TestUnit_Validate(t *testing.T) {
	tests := []struct {
		name    string
		mission *Mission
		wantErr bool
	}{
		{name: "valid open mission", mission: &Mission{Intent: "ship the board", Status: StatusOpen, HITLPolicyName: testPolicy}},
		{name: "valid landed mission", mission: &Mission{Intent: "ship the board", Status: StatusLanded, HITLPolicyName: testPolicy}},
		{name: "empty intent is rejected", mission: &Mission{Intent: "", Status: StatusOpen, HITLPolicyName: testPolicy}, wantErr: true},
		{name: "whitespace intent is rejected", mission: &Mission{Intent: "   ", Status: StatusOpen, HITLPolicyName: testPolicy}, wantErr: true},
		{name: "multi-line intent is rejected", mission: &Mission{Intent: "line one\nline two", Status: StatusOpen, HITLPolicyName: testPolicy}, wantErr: true},
		{name: "unknown status is rejected", mission: &Mission{Intent: "ok", Status: "bogus", HITLPolicyName: testPolicy}, wantErr: true},
		{name: "empty status is rejected", mission: &Mission{Intent: "ok", Status: "", HITLPolicyName: testPolicy}, wantErr: true},
		{name: "empty envelope is rejected", mission: &Mission{Intent: "ok", Status: StatusOpen, HITLPolicyName: ""}, wantErr: true},
		{name: "whitespace envelope is rejected", mission: &Mission{Intent: "ok", Status: StatusOpen, HITLPolicyName: "   "}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validate(tt.mission)
			if tt.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

// ─── Create / lifecycle ─────────────────────────────────────────────────────

func TestUnit_MissionService_CreateAssignsIDAndOpenStatus(t *testing.T) {
	ctx, db := setupMissionDB(t)
	svc := New(db)

	m := newMission("ship the fleet board")
	m.Status = StatusLanded // must be forced back to open on create
	require.NoError(t, svc.Create(ctx, m))

	require.NotEmpty(t, m.ID)
	_, err := uuid.Parse(m.ID)
	require.NoError(t, err)
	require.Equal(t, StatusOpen, m.Status)
	require.False(t, m.CreatedAt.IsZero())
	require.Equal(t, m.CreatedAt, m.UpdatedAt)
}

// TestUnit_MissionService_CreateLeavesBindAndLivenessFieldsZero pins that a fresh mission has no bind or heartbeat fields set.
func TestUnit_MissionService_CreateLeavesBindAndLivenessFieldsZero(t *testing.T) {
	ctx, db := setupMissionDB(t)
	svc := New(db)

	m := newMission("fresh mission")
	require.NoError(t, svc.Create(ctx, m))

	require.Empty(t, m.SessionID)
	require.Empty(t, m.InstanceID)
	require.Nil(t, m.LastHeartbeat)
	require.Empty(t, m.LastError)
}

func TestUnit_MissionService_CreateRejectsInvalidIntent(t *testing.T) {
	ctx, db := setupMissionDB(t)
	svc := New(db)

	require.Error(t, svc.Create(ctx, newMission("")))
	require.Error(t, svc.Create(ctx, newMission("two\nlines")))
}

// TestUnit_MissionService_CreateRejectsMissingEnvelope pins that Create rejects a missing or blank HITLPolicyName.
func TestUnit_MissionService_CreateRejectsMissingEnvelope(t *testing.T) {
	ctx, db := setupMissionDB(t)
	svc := New(db)

	require.Error(t, svc.Create(ctx, &Mission{Intent: "no envelope"}))
	require.Error(t, svc.Create(ctx, &Mission{Intent: "blank envelope", HITLPolicyName: "   "}))
}

func TestUnit_MissionService_CreateGetUpdateDelete(t *testing.T) {
	ctx, db := setupMissionDB(t)
	svc := New(db)

	m := newMission("crud mission")
	require.NoError(t, svc.Create(ctx, m))

	got, err := svc.Get(ctx, m.ID)
	require.NoError(t, err)
	require.Equal(t, m.Intent, got.Intent)
	require.Equal(t, "runner", got.AgentName)
	require.Equal(t, testPolicy, got.HITLPolicyName)
	require.Equal(t, StatusOpen, got.Status)

	got.Intent = "crud mission (edited)"
	require.NoError(t, svc.Update(ctx, got))

	updated, err := svc.Get(ctx, m.ID)
	require.NoError(t, err)
	require.Equal(t, "crud mission (edited)", updated.Intent)
	require.True(t, updated.CreatedAt.Equal(m.CreatedAt), "update must preserve createdAt")

	require.NoError(t, svc.Delete(ctx, m.ID))
	_, err = svc.Get(ctx, m.ID)
	require.Error(t, err)
	require.True(t, errors.Is(err, libdb.ErrNotFound))
}

func TestUnit_MissionService_List(t *testing.T) {
	ctx, db := setupMissionDB(t)
	svc := New(db)

	for _, intent := range []string{"mission-1", "mission-2", "mission-3"} {
		require.NoError(t, svc.Create(ctx, newMission(intent)))
	}

	items, err := svc.List(ctx, nil, 100)
	require.NoError(t, err)
	require.Len(t, items, 3)
}

func TestUnit_MissionService_ListEmptyIsNonNil(t *testing.T) {
	ctx, db := setupMissionDB(t)
	svc := New(db)

	items, err := svc.List(ctx, nil, 100)
	require.NoError(t, err)
	require.NotNil(t, items)
	require.Empty(t, items)
}

// TestUnit_MissionService_MissionOutlivesSessionAndStaysListed pins that a mission stays listed and open past session teardown.
func TestUnit_MissionService_MissionOutlivesSessionAndStaysListed(t *testing.T) {
	ctx, db := setupMissionDB(t)
	svc := New(db)

	m := newMission("long-running mission")
	require.NoError(t, svc.Create(ctx, m))
	_, err := svc.Bind(ctx, m.ID, "session-gone", "")
	require.NoError(t, err)

	items, err := svc.List(ctx, nil, 100)
	require.NoError(t, err)
	require.Len(t, items, 1)
	require.Equal(t, StatusOpen, items[0].Status)
}

// ─── Status transitions ─────────────────────────────────────────────────────

func TestUnit_MissionService_UpdateStatusTransitions(t *testing.T) {
	ctx, db := setupMissionDB(t)
	svc := New(db)

	m := newMission("transition mission")
	require.NoError(t, svc.Create(ctx, m))

	for _, status := range []Status{StatusLanded, StatusDerailed, StatusAbandoned, StatusOpen} {
		m.Status = status
		require.NoError(t, svc.Update(ctx, m), "status %q must be accepted", status)
		got, err := svc.Get(ctx, m.ID)
		require.NoError(t, err)
		require.Equal(t, status, got.Status)
	}
}

func TestUnit_MissionService_UpdateRejectsUnknownStatus(t *testing.T) {
	ctx, db := setupMissionDB(t)
	svc := New(db)

	m := newMission("bad-status mission")
	require.NoError(t, svc.Create(ctx, m))

	m.Status = "bogus"
	require.Error(t, svc.Update(ctx, m))
}

func TestUnit_MissionService_UpdateUnknownReturnsNotFound(t *testing.T) {
	ctx, db := setupMissionDB(t)
	svc := New(db)

	orphan := &Mission{ID: "no-such-id", Intent: "ghost", Status: StatusOpen, HITLPolicyName: testPolicy}
	err := svc.Update(ctx, orphan)
	require.Error(t, err)
	require.True(t, errors.Is(err, libdb.ErrNotFound))
}

func TestUnit_MissionService_GetUnknownReturnsNotFound(t *testing.T) {
	ctx, db := setupMissionDB(t)
	svc := New(db)

	_, err := svc.Get(ctx, "no-such-id")
	require.Error(t, err)
	require.True(t, errors.Is(err, libdb.ErrNotFound))
}

// ─── Bind ───────────────────────────────────────────────────────────────────

func TestUnit_MissionService_BindSetsSessionAndInstance(t *testing.T) {
	ctx, db := setupMissionDB(t)
	svc := New(db)

	m := newMission("bind mission")
	require.NoError(t, svc.Create(ctx, m))

	bound, err := svc.Bind(ctx, m.ID, "session-1", "instance-1")
	require.NoError(t, err)
	require.Equal(t, "session-1", bound.SessionID)
	require.Equal(t, "instance-1", bound.InstanceID)

	persisted, err := svc.Get(ctx, m.ID)
	require.NoError(t, err)
	require.Equal(t, "session-1", persisted.SessionID)
	require.Equal(t, "instance-1", persisted.InstanceID)
}

// TestUnit_MissionService_BindSameIDIsIdempotentNoOp pins that re-binding the same id is a true no-op (no restamp).
func TestUnit_MissionService_BindSameIDIsIdempotentNoOp(t *testing.T) {
	ctx, db := setupMissionDB(t)
	svc := New(db)

	m := newMission("idempotent bind")
	require.NoError(t, svc.Create(ctx, m))

	first, err := svc.Bind(ctx, m.ID, "session-1", "instance-1")
	require.NoError(t, err)
	firstUpdatedAt := first.UpdatedAt

	again, err := svc.Bind(ctx, m.ID, "session-1", "instance-1")
	require.NoError(t, err)
	require.Equal(t, "session-1", again.SessionID)
	require.Equal(t, "instance-1", again.InstanceID)
	require.Equal(t, firstUpdatedAt, again.UpdatedAt, "a true no-op must not restamp UpdatedAt")
}

// TestUnit_MissionService_BindConflictingSessionIsRejected pins that rebinding a different session id is a conflict, not an append.
func TestUnit_MissionService_BindConflictingSessionIsRejected(t *testing.T) {
	ctx, db := setupMissionDB(t)
	svc := New(db)

	m := newMission("conflicting session bind")
	require.NoError(t, svc.Create(ctx, m))

	_, err := svc.Bind(ctx, m.ID, "session-1", "")
	require.NoError(t, err)

	_, err = svc.Bind(ctx, m.ID, "session-2", "")
	require.Error(t, err)
	require.True(t, errors.Is(err, errdefs.ErrConflict))

	// The original binding must survive the rejected attempt.
	persisted, err := svc.Get(ctx, m.ID)
	require.NoError(t, err)
	require.Equal(t, "session-1", persisted.SessionID)
}

func TestUnit_MissionService_BindConflictingInstanceIsRejected(t *testing.T) {
	ctx, db := setupMissionDB(t)
	svc := New(db)

	m := newMission("conflicting instance bind")
	require.NoError(t, svc.Create(ctx, m))

	_, err := svc.Bind(ctx, m.ID, "", "instance-1")
	require.NoError(t, err)

	_, err = svc.Bind(ctx, m.ID, "", "instance-2")
	require.Error(t, err)
	require.True(t, errors.Is(err, errdefs.ErrConflict))

	persisted, err := svc.Get(ctx, m.ID)
	require.NoError(t, err)
	require.Equal(t, "instance-1", persisted.InstanceID)
}

func TestUnit_MissionService_BindRequiresAnID(t *testing.T) {
	ctx, db := setupMissionDB(t)
	svc := New(db)

	m := newMission("no-op bind")
	require.NoError(t, svc.Create(ctx, m))

	_, err := svc.Bind(ctx, m.ID, "", "")
	require.Error(t, err)
}

func TestUnit_MissionService_BindUnknownReturnsNotFound(t *testing.T) {
	ctx, db := setupMissionDB(t)
	svc := New(db)

	_, err := svc.Bind(ctx, "no-such-id", "session-1", "")
	require.Error(t, err)
	require.True(t, errors.Is(err, libdb.ErrNotFound))
}

// ─── Heartbeat ──────────────────────────────────────────────────────────────

func TestUnit_MissionService_HeartbeatUpdatesLastHeartbeatAndBumpsUpdatedAt(t *testing.T) {
	ctx, db := setupMissionDB(t)
	svc := New(db)

	m := newMission("heartbeat mission")
	require.NoError(t, svc.Create(ctx, m))
	require.Nil(t, m.LastHeartbeat)
	preUpdatedAt := m.UpdatedAt

	time.Sleep(1 * time.Millisecond)
	beat, err := svc.Heartbeat(ctx, m.ID, "")
	require.NoError(t, err)
	require.NotNil(t, beat.LastHeartbeat)
	require.True(t, beat.UpdatedAt.After(preUpdatedAt))

	persisted, err := svc.Get(ctx, m.ID)
	require.NoError(t, err)
	require.NotNil(t, persisted.LastHeartbeat)
	require.WithinDuration(t, *beat.LastHeartbeat, *persisted.LastHeartbeat, 0)
}

// TestUnit_MissionService_HeartbeatSetsAndClearsLastError pins that LastError sets and clears through Heartbeat.
func TestUnit_MissionService_HeartbeatSetsAndClearsLastError(t *testing.T) {
	ctx, db := setupMissionDB(t)
	svc := New(db)

	m := newMission("erroring mission")
	require.NoError(t, svc.Create(ctx, m))
	require.Empty(t, m.LastError)

	errored, err := svc.Heartbeat(ctx, m.ID, "tool exec failed: boom")
	require.NoError(t, err)
	require.Equal(t, "tool exec failed: boom", errored.LastError)

	persisted, err := svc.Get(ctx, m.ID)
	require.NoError(t, err)
	require.Equal(t, "tool exec failed: boom", persisted.LastError)

	cleared, err := svc.Heartbeat(ctx, m.ID, "")
	require.NoError(t, err)
	require.Empty(t, cleared.LastError)

	persisted, err = svc.Get(ctx, m.ID)
	require.NoError(t, err)
	require.Empty(t, persisted.LastError)
}

func TestUnit_MissionService_HeartbeatUnknownReturnsNotFound(t *testing.T) {
	ctx, db := setupMissionDB(t)
	svc := New(db)

	_, err := svc.Heartbeat(ctx, "no-such-id", "")
	require.Error(t, err)
	require.True(t, errors.Is(err, libdb.ErrNotFound))
}

// ─── validateReport() table test ───────────────────────────────────────────

func TestUnit_ValidateReport(t *testing.T) {
	tests := []struct {
		name    string
		report  *Report
		wantErr bool
	}{
		{name: "valid progress report", report: &Report{Kind: ReportKindProgress, Summary: "halfway done"}},
		{name: "valid finding report", report: &Report{Kind: ReportKindFinding, Summary: "found the bug"}},
		{name: "valid blocker report", report: &Report{Kind: ReportKindBlocker, Summary: "need credentials"}},
		{name: "valid result report", report: &Report{Kind: ReportKindResult, Summary: "shipped"}},
		{name: "unknown kind is rejected", report: &Report{Kind: "bogus", Summary: "ok"}, wantErr: true},
		{name: "empty kind is rejected", report: &Report{Kind: "", Summary: "ok"}, wantErr: true},
		{name: "empty summary is rejected", report: &Report{Kind: ReportKindProgress, Summary: ""}, wantErr: true},
		{name: "whitespace summary is rejected", report: &Report{Kind: ReportKindProgress, Summary: "   "}, wantErr: true},
		{name: "multi-line summary is rejected", report: &Report{Kind: ReportKindProgress, Summary: "line one\nline two"}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateReport(tt.report)
			if tt.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

// ─── AddReport / ListReports ────────────────────────────────────────────────

func TestUnit_MissionService_AddReportAssignsIDAndCreatedAt(t *testing.T) {
	ctx, db := setupMissionDB(t)
	svc := New(db)

	m := newMission("reported mission")
	require.NoError(t, svc.Create(ctx, m))

	rep := &Report{Kind: ReportKindProgress, Summary: "started work"}
	require.NoError(t, svc.AddReport(ctx, m.ID, rep))

	require.NotEmpty(t, rep.ID)
	_, err := uuid.Parse(rep.ID)
	require.NoError(t, err)
	require.Equal(t, m.ID, rep.MissionID)
	require.False(t, rep.CreatedAt.IsZero())
}

// TestUnit_MissionService_AddReportOverridesSuppliedMissionID pins that the missionID argument is authoritative over the report body.
func TestUnit_MissionService_AddReportOverridesSuppliedMissionID(t *testing.T) {
	ctx, db := setupMissionDB(t)
	svc := New(db)

	m := newMission("authoritative mission id")
	require.NoError(t, svc.Create(ctx, m))

	rep := &Report{MissionID: "some-other-mission", Kind: ReportKindProgress, Summary: "started work"}
	require.NoError(t, svc.AddReport(ctx, m.ID, rep))
	require.Equal(t, m.ID, rep.MissionID)
}

func TestUnit_MissionService_AddReportRejectsInvalidKind(t *testing.T) {
	ctx, db := setupMissionDB(t)
	svc := New(db)

	m := newMission("bad kind mission")
	require.NoError(t, svc.Create(ctx, m))

	err := svc.AddReport(ctx, m.ID, &Report{Kind: "bogus", Summary: "ok"})
	require.Error(t, err)
}

func TestUnit_MissionService_AddReportRejectsMultiLineSummary(t *testing.T) {
	ctx, db := setupMissionDB(t)
	svc := New(db)

	m := newMission("multi-line summary mission")
	require.NoError(t, svc.Create(ctx, m))

	err := svc.AddReport(ctx, m.ID, &Report{Kind: ReportKindProgress, Summary: "line one\nline two"})
	require.Error(t, err)
}

// TestUnit_MissionService_AddReportUnknownMissionReturnsNotFound pins that a report against an unknown mission is not-found, never a silent insert.
func TestUnit_MissionService_AddReportUnknownMissionReturnsNotFound(t *testing.T) {
	ctx, db := setupMissionDB(t)
	svc := New(db)

	err := svc.AddReport(ctx, "no-such-mission", &Report{Kind: ReportKindProgress, Summary: "orphan report"})
	require.Error(t, err)
	require.True(t, errors.Is(err, libdb.ErrNotFound))

	items, listErr := svc.ListReports(ctx, "no-such-mission", 100)
	require.NoError(t, listErr)
	require.Empty(t, items, "a rejected AddReport must not have inserted anything")
}

func TestUnit_MissionService_ListReportsEmptyIsNonNil(t *testing.T) {
	ctx, db := setupMissionDB(t)
	svc := New(db)

	m := newMission("no reports yet")
	require.NoError(t, svc.Create(ctx, m))

	items, err := svc.ListReports(ctx, m.ID, 100)
	require.NoError(t, err)
	require.NotNil(t, items)
	require.Empty(t, items)
}

func TestUnit_MissionService_ListReportsNewestFirst(t *testing.T) {
	ctx, db := setupMissionDB(t)
	svc := New(db)

	m := newMission("multi-report mission")
	require.NoError(t, svc.Create(ctx, m))

	summaries := []string{"first", "second", "third"}
	for _, summary := range summaries {
		require.NoError(t, svc.AddReport(ctx, m.ID, &Report{Kind: ReportKindProgress, Summary: summary}))
		time.Sleep(1 * time.Millisecond) // force distinct createdAt for a stable newest-first order
	}

	items, err := svc.ListReports(ctx, m.ID, 100)
	require.NoError(t, err)
	require.Len(t, items, 3)
	require.Equal(t, "third", items[0].Summary)
	require.Equal(t, "second", items[1].Summary)
	require.Equal(t, "first", items[2].Summary)
}

// TestUnit_MissionService_ListReportsScopedToOwnMission pins that reports never leak across missions or surface as missions.
func TestUnit_MissionService_ListReportsScopedToOwnMission(t *testing.T) {
	ctx, db := setupMissionDB(t)
	svc := New(db)

	a := newMission("mission a")
	require.NoError(t, svc.Create(ctx, a))
	b := newMission("mission b")
	require.NoError(t, svc.Create(ctx, b))

	require.NoError(t, svc.AddReport(ctx, a.ID, &Report{Kind: ReportKindProgress, Summary: "a-report-1"}))
	require.NoError(t, svc.AddReport(ctx, b.ID, &Report{Kind: ReportKindProgress, Summary: "b-report-1"}))
	require.NoError(t, svc.AddReport(ctx, b.ID, &Report{Kind: ReportKindResult, Summary: "b-report-2"}))

	aReports, err := svc.ListReports(ctx, a.ID, 100)
	require.NoError(t, err)
	require.Len(t, aReports, 1)
	require.Equal(t, "a-report-1", aReports[0].Summary)

	bReports, err := svc.ListReports(ctx, b.ID, 100)
	require.NoError(t, err)
	require.Len(t, bReports, 2)

	// Listing missions must never surface report rows as missions.
	missions, err := svc.List(ctx, nil, 100)
	require.NoError(t, err)
	require.Len(t, missions, 2)
}

// ─── GetByInstance: the mission-from-instance lookup ───────────────────────

func TestUnit_MissionService_GetByInstanceFindsTheBoundMission(t *testing.T) {
	ctx, db := setupMissionDB(t)
	svc := New(db)

	other := newMission("unrelated work")
	require.NoError(t, svc.Create(ctx, other))
	_, err := svc.Bind(ctx, other.ID, "sess-other", "inst-other")
	require.NoError(t, err)

	m := newMission("the work we are asking about")
	require.NoError(t, svc.Create(ctx, m))
	_, err = svc.Bind(ctx, m.ID, "sess-1", "inst-1")
	require.NoError(t, err)

	got, err := svc.GetByInstance(ctx, "inst-1")
	require.NoError(t, err)
	require.Equal(t, m.ID, got.ID)
	require.Equal(t, testPolicy, got.HITLPolicyName, "the envelope rides along on the lookup")
}

func TestUnit_MissionService_GetByInstanceUnknownIsNotFound(t *testing.T) {
	ctx, db := setupMissionDB(t)
	svc := New(db)

	m := newMission("bound elsewhere")
	require.NoError(t, svc.Create(ctx, m))
	_, err := svc.Bind(ctx, m.ID, "sess-1", "inst-1")
	require.NoError(t, err)

	// No mission claims it: a normal answer, not a failure.
	_, err = svc.GetByInstance(ctx, "inst-nobody-claimed")
	require.ErrorIs(t, err, libdb.ErrNotFound)
}

func TestUnit_MissionService_GetByInstanceRequiresAnID(t *testing.T) {
	ctx, db := setupMissionDB(t)
	svc := New(db)

	_, err := svc.GetByInstance(ctx, "")
	require.Error(t, err)
}

// TestUnit_MissionService_GetByInstanceDuplicateClaimIsDeterministic pins that a duplicate claim resolves to the newest, every time.
func TestUnit_MissionService_GetByInstanceDuplicateClaimIsDeterministic(t *testing.T) {
	ctx, db := setupMissionDB(t)
	svc := New(db)

	first := newMission("first claim")
	require.NoError(t, svc.Create(ctx, first))
	_, err := svc.Bind(ctx, first.ID, "sess-a", "inst-shared")
	require.NoError(t, err)

	time.Sleep(10 * time.Millisecond) // distinct created_at ordering
	second := newMission("second claim")
	require.NoError(t, svc.Create(ctx, second))
	_, err = svc.Bind(ctx, second.ID, "sess-b", "inst-shared")
	require.NoError(t, err)

	for i := 0; i < 3; i++ {
		got, err := svc.GetByInstance(ctx, "inst-shared")
		require.NoError(t, err)
		require.Equal(t, second.ID, got.ID, "the most recently created claim wins, every time")
	}
}

// TestUnit_MissionService_GetByInstanceScansPastOnePage pins that the scan pages: a mission past the first page is still found.
func TestUnit_MissionService_GetByInstanceScansPastOnePage(t *testing.T) {
	ctx, db := setupMissionDB(t)
	svc := New(db)

	target := newMission("the oldest mission")
	require.NoError(t, svc.Create(ctx, target))
	_, err := svc.Bind(ctx, target.ID, "sess-target", "inst-target")
	require.NoError(t, err)

	for i := 0; i < scanPageSize+5; i++ {
		filler := newMission("filler")
		require.NoError(t, svc.Create(ctx, filler))
	}

	got, err := svc.GetByInstance(ctx, "inst-target")
	require.NoError(t, err)
	require.Equal(t, target.ID, got.ID)
}

// ─── the supervision edge ──────────────────────────────────────────────────

// TestUnit_MissionService_ParentSessionIDRoundTrips pins that ParentSessionID (who fired it) survives and differs from SessionID (what it spawned).
func TestUnit_MissionService_ParentSessionIDRoundTrips(t *testing.T) {
	ctx, db := setupMissionDB(t)
	svc := New(db)

	fired := newMission("fired by an agent's session")
	fired.ParentSessionID = "upstream-session-9"
	require.NoError(t, svc.Create(ctx, fired))
	_, err := svc.Bind(ctx, fired.ID, "spawned-session", "spawned-instance")
	require.NoError(t, err)

	got, err := svc.Get(ctx, fired.ID)
	require.NoError(t, err)
	require.Equal(t, "upstream-session-9", got.ParentSessionID, "who FIRED it")
	require.Equal(t, "spawned-session", got.SessionID, "what it SPAWNED — a different fact")
	require.NotEqual(t, got.ParentSessionID, got.SessionID)

	direct := newMission("fired by an operator")
	require.NoError(t, svc.Create(ctx, direct))
	gotDirect, err := svc.Get(ctx, direct.ID)
	require.NoError(t, err)
	require.Empty(t, gotDirect.ParentSessionID, "no parent means an operator fired it directly")
}

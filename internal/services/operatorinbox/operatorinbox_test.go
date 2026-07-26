package operatorinbox

import (
	"context"
	"path/filepath"
	"testing"

	libdb "github.com/contenox/beam/internal/libdbexec"
	"github.com/contenox/beam/internal/services/missionservice"
	"github.com/contenox/beam/internal/store/runtimetypes"
	"github.com/stretchr/testify/require"
)

func setupInboxDB(t *testing.T) (context.Context, libdb.DBManager) {
	t.Helper()
	ctx := context.Background()
	db, err := libdb.NewSQLiteDBManager(ctx, filepath.Join(t.TempDir(), "operatorinbox.db"), runtimetypes.SchemaSQLite)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	return ctx, db
}

// TestUnit_Inbox_AddAndListRoundtrip stores an item and reads it back, asserting
// the self-contained snapshot (mission attribution + embedded report + reason)
// survives the roundtrip and that an id/timestamp are assigned when absent.
func TestUnit_Inbox_AddAndListRoundtrip(t *testing.T) {
	ctx, db := setupInboxDB(t)
	svc := New(db)

	item := &Item{
		MissionID: "m1",
		AgentName: "runner",
		Intent:    "do the thing",
		Reason:    ReasonOperatorFired,
		Report:    missionservice.Report{ID: "r1", MissionID: "m1", Kind: missionservice.ReportKindResult, Summary: "done"},
	}
	require.NoError(t, svc.Add(ctx, item))
	require.NotEmpty(t, item.ID, "Add assigns an id when absent")
	require.False(t, item.CreatedAt.IsZero(), "Add stamps CreatedAt when absent")

	items, err := svc.List(ctx, 100)
	require.NoError(t, err)
	require.Len(t, items, 1)
	got := items[0]
	require.Equal(t, item.ID, got.ID)
	require.Equal(t, "m1", got.MissionID)
	require.Equal(t, "runner", got.AgentName)
	require.Equal(t, ReasonOperatorFired, got.Reason)
	require.Equal(t, "done", got.Report.Summary)
	require.Equal(t, missionservice.ReportKindResult, got.Report.Kind)
}

// TestUnit_Inbox_ListNewestFirst asserts the list ordering an operator reads.
func TestUnit_Inbox_ListNewestFirst(t *testing.T) {
	ctx, db := setupInboxDB(t)
	svc := New(db)

	require.NoError(t, svc.Add(ctx, &Item{MissionID: "m1", Reason: ReasonOperatorFired,
		Report: missionservice.Report{Kind: missionservice.ReportKindProgress, Summary: "first"}}))
	require.NoError(t, svc.Add(ctx, &Item{MissionID: "m2", Reason: ReasonParentGone, ParentSessionID: "gone",
		Report: missionservice.Report{Kind: missionservice.ReportKindResult, Summary: "second"}}))

	items, err := svc.List(ctx, 100)
	require.NoError(t, err)
	require.Len(t, items, 2)
	require.Equal(t, "second", items[0].Report.Summary, "newest first")
	require.Equal(t, "first", items[1].Report.Summary)
	require.Equal(t, ReasonParentGone, items[0].Reason)
	require.Equal(t, "gone", items[0].ParentSessionID)
}

// TestUnit_Inbox_EmptyIsNonNil proves an empty inbox renders as [], not null.
func TestUnit_Inbox_EmptyIsNonNil(t *testing.T) {
	ctx, db := setupInboxDB(t)
	svc := New(db)
	items, err := svc.List(ctx, 100)
	require.NoError(t, err)
	require.NotNil(t, items)
	require.Empty(t, items)
}

// TestUnit_Inbox_AddValidates rejects the two ways an item is unusable: no
// mission to attribute it to, and an unknown reason.
func TestUnit_Inbox_AddValidates(t *testing.T) {
	ctx, db := setupInboxDB(t)
	svc := New(db)

	require.Error(t, svc.Add(ctx, &Item{Reason: ReasonOperatorFired}), "missionId is required")
	require.Error(t, svc.Add(ctx, &Item{MissionID: "m1", Reason: "bogus"}), "reason must be a known value")
	require.Error(t, svc.Add(ctx, nil))
}

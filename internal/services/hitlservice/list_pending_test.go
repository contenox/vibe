package hitlservice_test

import (
	"context"
	"testing"
	"time"

	"github.com/contenox/contenox/internal/services/hitlservice"
	"github.com/contenox/contenox/internal/store/runtimetypes"
	"github.com/contenox/contenox/libtracker"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// TestUnit_ListPending_ReturnsOnlyPendingNewestFirst pins that ListPending
// filters to state=pending and orders newest-first.
func TestUnit_ListPending_ReturnsOnlyPendingNewestFirst(t *testing.T) {
	t.Parallel()
	ctx, store, _ := setupHITLDB(t)
	svc := newDurableService(t, store)

	base := time.Now().UTC().Add(-time.Hour)
	var pendingIDs []string
	for i := 0; i < 3; i++ {
		row := seedPendingRow(t, ctx, store, "deny", base.Add(time.Duration(i)*time.Minute), base.Add(time.Hour))
		pendingIDs = append(pendingIDs, row.ID)
	}

	resolved := seedPendingRow(t, ctx, store, "deny", base, base.Add(time.Hour))
	require.NoError(t, svc.Respond(ctx, resolved.ID, true))

	got, err := svc.ListPending(ctx, 100)
	require.NoError(t, err)
	require.Len(t, got, 3, "the resolved row must not appear in the pending inbox")

	require.Equal(t, pendingIDs[2], got[0].ID)
	require.Equal(t, pendingIDs[1], got[1].ID)
	require.Equal(t, pendingIDs[0], got[2].ID)

	for _, row := range got {
		require.Equal(t, runtimetypes.HITLApprovalPending, row.State)
	}
}

// TestUnit_ListPendingForSession_ScopesToOneSessionsOpenAsks pins that
// ListPendingForSession returns only this session's still-pending rows,
// newest first, and empty for a quiet or unattributed session.
func TestUnit_ListPendingForSession_ScopesToOneSessionsOpenAsks(t *testing.T) {
	t.Parallel()
	ctx, store, _ := setupHITLDB(t)
	svc := newDurableService(t, store)

	base := time.Now().UTC().Add(-time.Hour)
	mine := seedPendingRowForSession(t, ctx, store, "sess-mine", base)
	newer := seedPendingRowForSession(t, ctx, store, "sess-mine", base.Add(time.Minute))
	seedPendingRowForSession(t, ctx, store, "sess-other", base)
	answered := seedPendingRowForSession(t, ctx, store, "sess-mine", base.Add(2*time.Minute))
	require.NoError(t, svc.Respond(ctx, answered.ID, true))

	got, err := svc.ListPendingForSession(ctx, "sess-mine", 100)
	require.NoError(t, err)
	require.Len(t, got, 2, "another session's ask and an answered one must not appear")
	require.Equal(t, newer.ID, got[0].ID, "newest first")
	require.Equal(t, mine.ID, got[1].ID)
	for _, row := range got {
		require.Equal(t, runtimetypes.HITLApprovalPending, row.State)
		require.Equal(t, "sess-mine", row.SessionID)
	}

	none, err := svc.ListPendingForSession(ctx, "sess-quiet", 100)
	require.NoError(t, err)
	require.Empty(t, none)
	unattributed, err := svc.ListPendingForSession(ctx, "", 100)
	require.NoError(t, err)
	require.Empty(t, unattributed)
}

func seedPendingRowForSession(t *testing.T, ctx context.Context, store runtimetypes.Store, sessionID string, createdAt time.Time) *runtimetypes.HITLApproval {
	t.Helper()
	row := &runtimetypes.HITLApproval{
		ID:        uuid.NewString(),
		ToolsName: "local_shell",
		ToolName:  "run_command",
		OnTimeout: "deny",
		State:     runtimetypes.HITLApprovalPending,
		SessionID: sessionID,
		CreatedAt: createdAt,
		ExpiresAt: createdAt.Add(time.Hour),
	}
	require.NoError(t, store.CreateHITLApproval(ctx, row))
	return row
}

// TestUnit_ListPending_CarriesTheDecisionFields pins that ListPending hands
// back the durable row itself, not a narrower projection.
func TestUnit_ListPending_CarriesTheDecisionFields(t *testing.T) {
	t.Parallel()
	ctx, store, _ := setupHITLDB(t)
	svc := newDurableService(t, store)

	now := time.Now().UTC()
	row := &runtimetypes.HITLApproval{
		ID:          "decision-fields",
		ToolsName:   "local_fs",
		ToolName:    "write_file",
		ArgsSummary: "/workspace/main.go",
		PolicyName:  "hitl-policy-default.json",
		OnTimeout:   "deny",
		State:       runtimetypes.HITLApprovalPending,
		CreatedAt:   now,
		ExpiresAt:   now.Add(time.Hour),
	}
	diff := "--- a\n+++ b\n"
	row.Diff = &diff
	rule := 3
	row.MatchedRule = &rule
	require.NoError(t, store.CreateHITLApproval(ctx, row))

	got, err := svc.ListPending(ctx, 100)
	require.NoError(t, err)
	require.Len(t, got, 1)

	gotRow := got[0]
	require.Equal(t, "local_fs", gotRow.ToolsName)
	require.Equal(t, "write_file", gotRow.ToolName)
	require.Equal(t, "/workspace/main.go", gotRow.ArgsSummary)
	require.NotNil(t, gotRow.Diff)
	require.Equal(t, diff, *gotRow.Diff)
	require.Equal(t, "hitl-policy-default.json", gotRow.PolicyName)
	require.NotNil(t, gotRow.MatchedRule)
	require.Equal(t, 3, *gotRow.MatchedRule)
	require.WithinDuration(t, row.CreatedAt, gotRow.CreatedAt, time.Second)
	require.WithinDuration(t, row.ExpiresAt, gotRow.ExpiresAt, time.Second)
}

// TestUnit_ListPending_RespectsLimit pins that limit bounds the result.
func TestUnit_ListPending_RespectsLimit(t *testing.T) {
	t.Parallel()
	ctx, store, _ := setupHITLDB(t)
	svc := newDurableService(t, store)

	base := time.Now().UTC().Add(-time.Hour)
	for i := 0; i < 5; i++ {
		seedPendingRow(t, ctx, store, "deny", base.Add(time.Duration(i)*time.Minute), base.Add(time.Hour))
	}

	got, err := svc.ListPending(ctx, 2)
	require.NoError(t, err)
	require.Len(t, got, 2)
}

// TestUnit_ListPending_EmptyIsNonNil pins that an empty inbox is a non-nil empty slice.
func TestUnit_ListPending_EmptyIsNonNil(t *testing.T) {
	t.Parallel()
	ctx, store, _ := setupHITLDB(t)
	svc := newDurableService(t, store)

	got, err := svc.ListPending(ctx, 100)
	require.NoError(t, err)
	require.NotNil(t, got)
	require.Empty(t, got)
}

// TestUnit_ListPending_ZeroLimitUsesStoreDefault pins that limit<=0 defers to the store's own default.
func TestUnit_ListPending_ZeroLimitUsesStoreDefault(t *testing.T) {
	t.Parallel()
	ctx, store, _ := setupHITLDB(t)
	svc := newDurableService(t, store)

	seedPendingRow(t, ctx, store, "deny", time.Now().UTC(), time.Now().UTC().Add(time.Hour))

	got, err := svc.ListPending(ctx, 0)
	require.NoError(t, err)
	require.Len(t, got, 1)
}

// TestUnit_ListPending_WithoutDurableStoreErrors pins that a bare-KVReader
// service fails loudly rather than reporting a silently empty inbox.
func TestUnit_ListPending_WithoutDurableStoreErrors(t *testing.T) {
	t.Parallel()
	svc := hitlservice.New(hitlservice.NewFSPolicySource(t.TempDir()), testTenant, fixedKVReader{"hitl-policy.json"}, libtracker.NoopTracker{})

	_, err := svc.ListPending(t.Context(), 100)
	require.Error(t, err)
}

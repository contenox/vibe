package runtimetypes_test

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"

	libdb "github.com/contenox/contenox/internal/libdbexec"
	"github.com/contenox/contenox/internal/store/runtimetypes"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// setupHITLApprovalsStore opens a fresh SQLite-backed Store (the production
// "Contenox Local" backend, and the one runtime/hitlservice's own tests use)
// rather than the Postgres-testcontainer SetupStore other _test.go files in
// this package use, so this file's tests need no Docker.
func setupHITLApprovalsStore(t *testing.T) (context.Context, runtimetypes.Store) {
	t.Helper()
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "hitl_approvals.db")
	db, err := libdb.NewSQLiteDBManager(ctx, dbPath, runtimetypes.SchemaSQLite)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	return ctx, runtimetypes.New(db.WithoutTransaction())
}

func newPendingApproval() *runtimetypes.HITLApproval {
	now := time.Now().UTC()
	return &runtimetypes.HITLApproval{
		ID:          uuid.NewString(),
		ToolsName:   "local_fs",
		ToolName:    "write_file",
		ArgsSummary: "/workspace/main.go",
		PolicyName:  "hitl-policy-default.json",
		OnTimeout:   "deny",
		State:       runtimetypes.HITLApprovalPending,
		CreatedAt:   now,
		ExpiresAt:   now.Add(time.Hour),
	}
}

func TestUnit_HITLApprovals_CreateAndGet(t *testing.T) {
	t.Parallel()
	ctx, s := setupHITLApprovalsStore(t)

	a := newPendingApproval()
	diff := "--- a\n+++ b\n"
	a.Diff = &diff
	rule := 2
	a.MatchedRule = &rule
	require.NoError(t, s.CreateHITLApproval(ctx, a))

	got, err := s.GetHITLApproval(ctx, a.ID)
	require.NoError(t, err)
	require.Equal(t, a.ID, got.ID)
	require.Equal(t, "local_fs", got.ToolsName)
	require.Equal(t, "write_file", got.ToolName)
	require.Equal(t, "/workspace/main.go", got.ArgsSummary)
	require.NotNil(t, got.Diff)
	require.Equal(t, diff, *got.Diff)
	require.Equal(t, "hitl-policy-default.json", got.PolicyName)
	require.NotNil(t, got.MatchedRule)
	require.Equal(t, 2, *got.MatchedRule)
	require.Equal(t, "deny", got.OnTimeout)
	require.Equal(t, runtimetypes.HITLApprovalPending, got.State)
	require.Nil(t, got.Resolution, "a pending row must have no resolution yet")
	require.Nil(t, got.ResolvedAt)
	require.WithinDuration(t, a.CreatedAt, got.CreatedAt, time.Second)
	require.WithinDuration(t, a.ExpiresAt, got.ExpiresAt, time.Second)
}

func TestUnit_HITLApprovals_CreateDefaultsEmptyStateToPending(t *testing.T) {
	t.Parallel()
	ctx, s := setupHITLApprovalsStore(t)

	a := newPendingApproval()
	a.State = "" // deliberately unset
	require.NoError(t, s.CreateHITLApproval(ctx, a))

	got, err := s.GetHITLApproval(ctx, a.ID)
	require.NoError(t, err)
	require.Equal(t, runtimetypes.HITLApprovalPending, got.State)
}

func TestUnit_HITLApprovals_GetUnknownReturnsNotFound(t *testing.T) {
	t.Parallel()
	ctx, s := setupHITLApprovalsStore(t)

	_, err := s.GetHITLApproval(ctx, "no-such-id")
	require.Error(t, err)
	require.True(t, errors.Is(err, libdb.ErrNotFound))
}

// ─── ResolveHITLApproval: the compare-and-swap ─────────────────────────────

func TestUnit_ResolveHITLApproval_TransitionsPendingRow(t *testing.T) {
	t.Parallel()
	ctx, s := setupHITLApprovalsStore(t)

	a := newPendingApproval()
	require.NoError(t, s.CreateHITLApproval(ctx, a))

	resolvedAt := time.Now().UTC()
	resolution := json.RawMessage(`{"approved":true}`)
	require.NoError(t, s.ResolveHITLApproval(ctx, a.ID, runtimetypes.HITLApprovalApproved, resolution, resolvedAt))

	got, err := s.GetHITLApproval(ctx, a.ID)
	require.NoError(t, err)
	require.Equal(t, runtimetypes.HITLApprovalApproved, got.State)
	require.NotNil(t, got.ResolvedAt)
	require.WithinDuration(t, resolvedAt, *got.ResolvedAt, time.Second)
	require.JSONEq(t, `{"approved":true}`, string(got.Resolution))
}

func TestUnit_ResolveHITLApproval_NilResolutionStoresNull(t *testing.T) {
	t.Parallel()
	ctx, s := setupHITLApprovalsStore(t)

	a := newPendingApproval()
	require.NoError(t, s.CreateHITLApproval(ctx, a))
	require.NoError(t, s.ResolveHITLApproval(ctx, a.ID, runtimetypes.HITLApprovalExpired, nil, time.Now().UTC()))

	got, err := s.GetHITLApproval(ctx, a.ID)
	require.NoError(t, err)
	require.Equal(t, runtimetypes.HITLApprovalExpired, got.State)
	require.Nil(t, got.Resolution)
}

func TestUnit_ResolveHITLApproval_UnknownIDReturnsNotFound(t *testing.T) {
	t.Parallel()
	ctx, s := setupHITLApprovalsStore(t)

	err := s.ResolveHITLApproval(ctx, "no-such-id", runtimetypes.HITLApprovalApproved, nil, time.Now().UTC())
	require.Error(t, err)
	require.True(t, errors.Is(err, libdb.ErrNotFound))
}

// TestUnit_ResolveHITLApproval_AlreadyResolvedIsRejected is the compare-and-
// swap guarantee itself: a second resolve attempt against a row that is no
// longer 'pending' must not succeed and must not change the row — this is
// what lets hitlservice's sweeper and Respond race safely against each
// other, whichever gets there first wins and the other sees ErrNotFound.
func TestUnit_ResolveHITLApproval_AlreadyResolvedIsRejected(t *testing.T) {
	t.Parallel()
	ctx, s := setupHITLApprovalsStore(t)

	a := newPendingApproval()
	require.NoError(t, s.CreateHITLApproval(ctx, a))

	firstResolvedAt := time.Now().UTC()
	require.NoError(t, s.ResolveHITLApproval(ctx, a.ID, runtimetypes.HITLApprovalApproved, json.RawMessage(`{"approved":true}`), firstResolvedAt))

	err := s.ResolveHITLApproval(ctx, a.ID, runtimetypes.HITLApprovalDenied, json.RawMessage(`{"approved":false}`), time.Now().UTC())
	require.Error(t, err)
	require.True(t, errors.Is(err, libdb.ErrNotFound))

	// The first resolution must stand, untouched by the rejected second call.
	got, err := s.GetHITLApproval(ctx, a.ID)
	require.NoError(t, err)
	require.Equal(t, runtimetypes.HITLApprovalApproved, got.State)
	require.JSONEq(t, `{"approved":true}`, string(got.Resolution))
	require.WithinDuration(t, firstResolvedAt, *got.ResolvedAt, time.Second)
}

// ─── ListExpiredHITLApprovals ───────────────────────────────────────────────

func TestUnit_ListExpiredHITLApprovals_ReturnsOnlyPastDeadlinePendingRows(t *testing.T) {
	t.Parallel()
	ctx, s := setupHITLApprovalsStore(t)
	now := time.Now().UTC()

	expired := newPendingApproval()
	expired.ExpiresAt = now.Add(-time.Minute)
	require.NoError(t, s.CreateHITLApproval(ctx, expired))

	notYetExpired := newPendingApproval()
	notYetExpired.ExpiresAt = now.Add(time.Hour)
	require.NoError(t, s.CreateHITLApproval(ctx, notYetExpired))

	alreadyResolved := newPendingApproval()
	alreadyResolved.ExpiresAt = now.Add(-time.Minute)
	require.NoError(t, s.CreateHITLApproval(ctx, alreadyResolved))
	require.NoError(t, s.ResolveHITLApproval(ctx, alreadyResolved.ID, runtimetypes.HITLApprovalApproved, json.RawMessage(`{"approved":true}`), now))

	got, err := s.ListExpiredHITLApprovals(ctx, now, 100)
	require.NoError(t, err)
	require.Len(t, got, 1, "only the pending row past its deadline must be returned")
	require.Equal(t, expired.ID, got[0].ID)
}

func TestUnit_ListExpiredHITLApprovals_EmptyIsNonNil(t *testing.T) {
	t.Parallel()
	ctx, s := setupHITLApprovalsStore(t)

	got, err := s.ListExpiredHITLApprovals(ctx, time.Now().UTC(), 100)
	require.NoError(t, err)
	require.NotNil(t, got)
	require.Empty(t, got)
}

// ─── ListHITLApprovals ──────────────────────────────────────────────────────

func TestUnit_ListHITLApprovals_FiltersByStateNewestFirst(t *testing.T) {
	t.Parallel()
	ctx, s := setupHITLApprovalsStore(t)
	base := time.Now().UTC().Add(-time.Hour)

	var pendingIDs []string
	for i := 0; i < 3; i++ {
		a := newPendingApproval()
		a.CreatedAt = base.Add(time.Duration(i) * time.Minute)
		require.NoError(t, s.CreateHITLApproval(ctx, a))
		pendingIDs = append(pendingIDs, a.ID)
	}
	resolved := newPendingApproval()
	require.NoError(t, s.CreateHITLApproval(ctx, resolved))
	require.NoError(t, s.ResolveHITLApproval(ctx, resolved.ID, runtimetypes.HITLApprovalDenied, json.RawMessage(`{"approved":false}`), time.Now().UTC()))

	got, err := s.ListHITLApprovals(ctx, runtimetypes.HITLApprovalPending, nil, 100)
	require.NoError(t, err)
	require.Len(t, got, 3)
	// newest first
	require.Equal(t, pendingIDs[2], got[0].ID)
	require.Equal(t, pendingIDs[0], got[2].ID)

	deniedOnly, err := s.ListHITLApprovals(ctx, runtimetypes.HITLApprovalDenied, nil, 100)
	require.NoError(t, err)
	require.Len(t, deniedOnly, 1)
	require.Equal(t, resolved.ID, deniedOnly[0].ID)
}

func TestUnit_EstimateHITLApprovalCount(t *testing.T) {
	t.Parallel()
	ctx, s := setupHITLApprovalsStore(t)

	for i := 0; i < 3; i++ {
		require.NoError(t, s.CreateHITLApproval(ctx, newPendingApproval()))
	}

	count, err := s.EstimateHITLApprovalCount(ctx)
	require.NoError(t, err)
	require.Equal(t, int64(3), count)
}

// ─── restart durability at the store layer ─────────────────────────────────

// TestUnit_HITLApprovals_RowSurvivesReopeningTheDatabase pins durability: a
// pending row written by one DBManager instance is visible, unchanged, to a
// separate DBManager instance opened later against the same file, simulating
// a `contenox serve` restart with no in-memory state carried over.
func TestUnit_HITLApprovals_RowSurvivesReopeningTheDatabase(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "restart.db")

	db1, err := libdb.NewSQLiteDBManager(ctx, dbPath, runtimetypes.SchemaSQLite)
	require.NoError(t, err)
	store1 := runtimetypes.New(db1.WithoutTransaction())

	a := newPendingApproval()
	require.NoError(t, store1.CreateHITLApproval(ctx, a))
	require.NoError(t, db1.Close())

	db2, err := libdb.NewSQLiteDBManager(ctx, dbPath, runtimetypes.SchemaSQLite)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db2.Close() })
	store2 := runtimetypes.New(db2.WithoutTransaction())

	got, err := store2.GetHITLApproval(ctx, a.ID)
	require.NoError(t, err)
	require.Equal(t, runtimetypes.HITLApprovalPending, got.State)
	require.Equal(t, a.ToolsName, got.ToolsName)
	require.Equal(t, a.ToolName, got.ToolName)

	// And it is resolvable from the "restarted" instance.
	require.NoError(t, store2.ResolveHITLApproval(ctx, a.ID, runtimetypes.HITLApprovalApproved, json.RawMessage(`{"approved":true}`), time.Now().UTC()))
	resolved, err := store2.GetHITLApproval(ctx, a.ID)
	require.NoError(t, err)
	require.Equal(t, runtimetypes.HITLApprovalApproved, resolved.State)
}

// ─── attribution ───────────────────────────────────────────────────────────
//
// The attribution columns name who is asking, not just which tool was
// called. These pin that they round-trip through every read path, and that
// MissionID stays nullable — no mission must be distinguishable from unknown.

func TestUnit_HITLApprovals_AttributionRoundTrips(t *testing.T) {
	t.Parallel()
	ctx, s := setupHITLApprovalsStore(t)

	missionID := uuid.NewString()
	a := newPendingApproval()
	a.InstanceID = "instance-42"
	a.SessionID = "sess_downstream_1"
	a.AgentName = "reviewer"
	a.MissionID = &missionID
	require.NoError(t, s.CreateHITLApproval(ctx, a))

	got, err := s.GetHITLApproval(ctx, a.ID)
	require.NoError(t, err)
	require.Equal(t, "instance-42", got.InstanceID)
	require.Equal(t, "sess_downstream_1", got.SessionID)
	require.Equal(t, "reviewer", got.AgentName)
	require.NotNil(t, got.MissionID)
	require.Equal(t, missionID, *got.MissionID)

	// The LIST paths project the same columns — the inbox reads through these,
	// not through Get.
	listed, err := s.ListHITLApprovals(ctx, runtimetypes.HITLApprovalPending, nil, 10)
	require.NoError(t, err)
	require.Len(t, listed, 1)
	require.Equal(t, "instance-42", listed[0].InstanceID)
	require.Equal(t, "reviewer", listed[0].AgentName)
	require.NotNil(t, listed[0].MissionID)
	require.Equal(t, missionID, *listed[0].MissionID)

	expired, err := s.ListExpiredHITLApprovals(ctx, a.ExpiresAt.Add(time.Minute), 10)
	require.NoError(t, err)
	require.Len(t, expired, 1)
	require.Equal(t, "sess_downstream_1", expired[0].SessionID)
	require.NotNil(t, expired[0].MissionID)
}

func TestUnit_HITLApprovals_UnattributedRowIsEmptyNotNull(t *testing.T) {
	t.Parallel()
	ctx, s := setupHITLApprovalsStore(t)

	// An ask raised by a native chain turn with no fleet unit behind it: no
	// attribution at all, which must store and read back cleanly rather than
	// failing a NOT NULL constraint or scanning into a nil string.
	a := newPendingApproval()
	require.NoError(t, s.CreateHITLApproval(ctx, a))

	got, err := s.GetHITLApproval(ctx, a.ID)
	require.NoError(t, err)
	require.Empty(t, got.InstanceID)
	require.Empty(t, got.SessionID)
	require.Empty(t, got.AgentName)
	require.Nil(t, got.MissionID, "no mission must read back as NULL, not as an empty string")
}

// attentionAsk builds a pending attention ask on missionID, in the shape
// hitlservice writes them (the marks ResolveHITLApprovalWithinBound counts on).
func attentionAsk(missionID string) *runtimetypes.HITLApproval {
	a := newPendingApproval()
	a.ToolsName, a.ToolName = "mission", "mission_ask_attention"
	a.ArgsSummary = "which branch?"
	a.MissionID = &missionID
	return a
}

// agentAnswerBound is the predicate hitlservice passes for missionID.
func agentAnswerBound(missionID string, max int) runtimetypes.AgentAnswerBound {
	return runtimetypes.AgentAnswerBound{
		MissionID:      missionID,
		ToolsName:      "mission",
		ToolName:       "mission_ask_attention",
		ResolutionLike: `%"answeredBy":%`,
		Max:            max,
	}
}

// TestUnit_HITLApprovals_ResolveWithinBoundStopsAtTheCap pins the count-and-write
// as one statement: the write itself refuses once the mission holds Max
// agent-answered asks, so a caller that counted before any of them landed
// cannot spend budget that is gone.
func TestUnit_HITLApprovals_ResolveWithinBoundStopsAtTheCap(t *testing.T) {
	t.Parallel()
	ctx, s := setupHITLApprovalsStore(t)
	const missionID = "m-bounded"
	const max = 2

	rows := make([]*runtimetypes.HITLApproval, 0, 5)
	for i := 0; i < 5; i++ {
		a := attentionAsk(missionID)
		require.NoError(t, s.CreateHITLApproval(ctx, a))
		rows = append(rows, a)
	}

	landed := 0
	for _, a := range rows {
		err := s.ResolveHITLApprovalWithinBound(ctx, a.ID, agentAnswerBound(missionID, max),
			runtimetypes.HITLApprovalApproved, json.RawMessage(`{"answer":"main","answeredBy":"oracle"}`), time.Now().UTC())
		if err == nil {
			landed++
			continue
		}
		require.ErrorIs(t, err, libdb.ErrNotFound, "a spent bound refuses like a lost CAS does")
		got, getErr := s.GetHITLApproval(ctx, a.ID)
		require.NoError(t, getErr)
		require.Equal(t, runtimetypes.HITLApprovalPending, got.State,
			"the row is untouched, which is how a caller tells a spent bound from an already-answered ask")
	}
	require.Equal(t, max, landed)
}

// TestUnit_HITLApprovals_ResolveWithinBoundCountsOnlyAgentAnswers pins the two
// halves of the predicate: a human's answer on the same mission spends no
// budget, and another mission's agent answers spend none of this one's.
func TestUnit_HITLApprovals_ResolveWithinBoundCountsOnlyAgentAnswers(t *testing.T) {
	t.Parallel()
	ctx, s := setupHITLApprovalsStore(t)
	const missionID = "m-mixed"
	now := time.Now().UTC()

	human := attentionAsk(missionID)
	require.NoError(t, s.CreateHITLApproval(ctx, human))
	require.NoError(t, s.ResolveHITLApproval(ctx, human.ID, runtimetypes.HITLApprovalApproved,
		json.RawMessage(`{"answer":"a human said answeredBy is not a key here"}`), now))

	other := attentionAsk("m-elsewhere")
	require.NoError(t, s.CreateHITLApproval(ctx, other))
	require.NoError(t, s.ResolveHITLApprovalWithinBound(ctx, other.ID, agentAnswerBound("m-elsewhere", 1),
		runtimetypes.HITLApprovalApproved, json.RawMessage(`{"answer":"x","answeredBy":"oracle"}`), now))

	agent := attentionAsk(missionID)
	require.NoError(t, s.CreateHITLApproval(ctx, agent))
	require.NoError(t, s.ResolveHITLApprovalWithinBound(ctx, agent.ID, agentAnswerBound(missionID, 1),
		runtimetypes.HITLApprovalApproved, json.RawMessage(`{"answer":"main","answeredBy":"oracle"}`), now),
		"neither the human answer nor another mission's agent answer counts against this cap")

	next := attentionAsk(missionID)
	require.NoError(t, s.CreateHITLApproval(ctx, next))
	require.ErrorIs(t, s.ResolveHITLApprovalWithinBound(ctx, next.ID, agentAnswerBound(missionID, 1),
		runtimetypes.HITLApprovalApproved, json.RawMessage(`{"answer":"main","answeredBy":"oracle"}`), now),
		libdb.ErrNotFound, "the one agent answer that does count spends the cap")
}

// TestUnit_HITLApprovals_ResolveWithinBoundKeepsThePendingCAS pins that the
// bound is an EXTRA predicate, not a replacement: a row already resolved stays
// single-winner even with budget to spare.
func TestUnit_HITLApprovals_ResolveWithinBoundKeepsThePendingCAS(t *testing.T) {
	t.Parallel()
	ctx, s := setupHITLApprovalsStore(t)
	const missionID = "m-cas"
	now := time.Now().UTC()

	a := attentionAsk(missionID)
	require.NoError(t, s.CreateHITLApproval(ctx, a))
	require.NoError(t, s.ResolveHITLApproval(ctx, a.ID, runtimetypes.HITLApprovalExpired, nil, now))

	require.ErrorIs(t, s.ResolveHITLApprovalWithinBound(ctx, a.ID, agentAnswerBound(missionID, 5),
		runtimetypes.HITLApprovalApproved, json.RawMessage(`{"answer":"late","answeredBy":"oracle"}`), now),
		libdb.ErrNotFound)

	got, err := s.GetHITLApproval(ctx, a.ID)
	require.NoError(t, err)
	require.Equal(t, runtimetypes.HITLApprovalExpired, got.State)
}

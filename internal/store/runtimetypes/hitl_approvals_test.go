package runtimetypes_test

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/contenox/contenox/internal/store/runtimetypes"
	libdb "github.com/contenox/contenox/libdbexec"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func setupHITLApprovalsStore(t *testing.T) (context.Context, runtimetypes.Store) {
	t.Helper()
	return runtimetypes.SetupStore(t)
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

// TestUnit_ResolveHITLApproval_AlreadyResolvedIsRejected verifies a second resolve on a non-pending row neither succeeds nor changes the row.
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

	got, err := s.GetHITLApproval(ctx, a.ID)
	require.NoError(t, err)
	require.Equal(t, runtimetypes.HITLApprovalApproved, got.State)
	require.JSONEq(t, `{"approved":true}`, string(got.Resolution))
	require.WithinDuration(t, firstResolvedAt, *got.ResolvedAt, time.Second)
}

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

// TestUnit_ListExpiredHITLApprovals_SkipsRowsWithNoDeadline pins the SQL that
// makes "wait until somebody answers" durable: a zero expires_at is outside
// the sweep's range, however old the row is, so no background read can ever
// resolve it on the operator's behalf.
func TestUnit_ListExpiredHITLApprovals_SkipsRowsWithNoDeadline(t *testing.T) {
	t.Parallel()
	ctx, s := setupHITLApprovalsStore(t)
	now := time.Now().UTC()

	noDeadline := newPendingApproval()
	noDeadline.CreatedAt = now.Add(-90 * 24 * time.Hour)
	noDeadline.ExpiresAt = time.Time{}
	require.NoError(t, s.CreateHITLApproval(ctx, noDeadline))

	expired := newPendingApproval()
	expired.ExpiresAt = now.Add(-time.Minute)
	require.NoError(t, s.CreateHITLApproval(ctx, expired))

	got, err := s.ListExpiredHITLApprovals(ctx, now, 100)
	require.NoError(t, err)
	require.Len(t, got, 1, "an ask with no deadline is not an expired ask")
	require.Equal(t, expired.ID, got[0].ID)

	// Even asked about a moment far in the future.
	got, err = s.ListExpiredHITLApprovals(ctx, now.Add(365*24*time.Hour), 100)
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Equal(t, expired.ID, got[0].ID)

	// ...and it reads back as the deadline-less row it was written as.
	back, err := s.GetHITLApproval(ctx, noDeadline.ID)
	require.NoError(t, err)
	require.True(t, back.ExpiresAt.IsZero())
	require.Equal(t, runtimetypes.HITLApprovalPending, back.State)
}

func TestUnit_ListExpiredHITLApprovals_EmptyIsNonNil(t *testing.T) {
	t.Parallel()
	ctx, s := setupHITLApprovalsStore(t)

	got, err := s.ListExpiredHITLApprovals(ctx, time.Now().UTC(), 100)
	require.NoError(t, err)
	require.NotNil(t, got)
	require.Empty(t, got)
}

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

// TestUnit_HITLApprovals_RowSurvivesReopeningTheDatabase verifies a pending row survives being reopened by a separate DBManager instance against the same file.
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

	require.NoError(t, store2.ResolveHITLApproval(ctx, a.ID, runtimetypes.HITLApprovalApproved, json.RawMessage(`{"approved":true}`), time.Now().UTC()))
	resolved, err := store2.GetHITLApproval(ctx, a.ID)
	require.NoError(t, err)
	require.Equal(t, runtimetypes.HITLApprovalApproved, resolved.State)
}

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

	a := newPendingApproval()
	require.NoError(t, s.CreateHITLApproval(ctx, a))

	got, err := s.GetHITLApproval(ctx, a.ID)
	require.NoError(t, err)
	require.Empty(t, got.InstanceID)
	require.Empty(t, got.SessionID)
	require.Empty(t, got.AgentName)
	require.Nil(t, got.MissionID, "no mission must read back as NULL, not as an empty string")
}

func attentionAsk(missionID string) *runtimetypes.HITLApproval {
	a := newPendingApproval()
	a.ToolsName, a.ToolName = "mission", "mission_ask_attention"
	a.ArgsSummary = "which branch?"
	a.MissionID = &missionID
	return a
}

func agentAnswerBound(missionID string, max int) runtimetypes.AgentAnswerBound {
	return runtimetypes.AgentAnswerBound{
		MissionID:      missionID,
		ToolsName:      "mission",
		ToolName:       "mission_ask_attention",
		ResolutionLike: `%"answeredBy":%`,
		Max:            max,
	}
}

// TestUnit_HITLApprovals_ResolveWithinBoundStopsAtTheCap verifies the write refuses once the mission holds Max agent-answered asks.
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

// TestUnit_HITLApprovals_ResolveWithinBoundCountsOnlyAgentAnswers verifies a human's answer spends no budget and another mission's agent answers spend none of this one's.
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

// TestUnit_HITLApprovals_ResolveWithinBoundKeepsThePendingCAS verifies a row already resolved stays single-winner even with budget to spare.
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

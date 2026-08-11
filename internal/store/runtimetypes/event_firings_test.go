package runtimetypes_test

// event_firings store tests, over the production SQLite backend (same
// no-Docker idiom as hitl_approvals_test.go).

import (
	"context"
	"testing"
	"time"

	"github.com/contenox/contenox/internal/store/runtimetypes"
	"github.com/stretchr/testify/require"
)

// seedFiring records one finished firing, the exact pair the dispatcher writes.
func seedEventFiring(t *testing.T, s runtimetypes.EventFiringStore, trigger string, nid int64, status, errMsg string) {
	t.Helper()
	ctx := context.Background()
	claimed, err := s.BeginEventFiring(ctx, trigger, nid, "evt-test")
	require.NoError(t, err)
	require.True(t, claimed)
	require.NoError(t, s.FinishEventFiring(ctx, trigger, nid, status, errMsg))
}

// eventFiringNIDs projects a listing to its nids, in listing order.
func eventFiringNIDs(firings []runtimetypes.EventFiring) []int64 {
	nids := make([]int64, 0, len(firings))
	for _, f := range firings {
		nids = append(nids, f.NID)
	}
	return nids
}

// TestUnit_EventFirings_StaleRunningClaimIsReclaimable pins the bound: a
// running claim is exclusive for exactly StaleEventFiringClaim, then a later
// host takes it over. This is what keeps a firing whose host died mid-run from
// being lost forever — the claim is INSERT OR IGNORE, so without the takeover a
// stranded 'running' row refuses every retry for good.
func TestUnit_EventFirings_StaleRunningClaimIsReclaimable(t *testing.T) {
	ctx := context.Background()
	db := setupEventDB(t)
	claimedAt := time.Now().UTC()
	at := func(d time.Duration) runtimetypes.EventFiringStore {
		return mustFiringStore(t, db.WithoutTransaction(), testEventWS,
			runtimetypes.WithEventFiringClock(func() time.Time { return claimedAt.Add(d) }))
	}

	claimed, err := at(0).BeginEventFiring(ctx, "on-report", 1, "evt-first")
	require.NoError(t, err)
	require.True(t, claimed)

	// Inside the bound the claim is exclusive: a live host that is merely slow
	// must never have its firing stolen and run a second time.
	claimed, err = at(runtimetypes.StaleEventFiringClaim).BeginEventFiring(ctx, "on-report", 1, "evt-too-early")
	require.NoError(t, err)
	require.False(t, claimed, "the bound is exclusive at its own edge")

	// Past it, the claim is taken over — the host is presumed dead.
	claimed, err = at(runtimetypes.StaleEventFiringClaim+time.Second).BeginEventFiring(ctx, "on-report", 1, "evt-retry")
	require.NoError(t, err)
	require.True(t, claimed, "a claim nothing has touched for the bound is a dead host's, not a live one's")

	firings, err := at(0).ListEventFirings(ctx, runtimetypes.EventFiringFilter{TriggerName: "on-report"})
	require.NoError(t, err)
	require.Len(t, firings, 1, "the takeover reuses the row; a retry is never a second firing record")
	require.Equal(t, runtimetypes.EventFiringStatusRunning, firings[0].Status)
	require.Equal(t, "evt-retry", firings[0].RequestID, "the row now names the run that actually holds it")
	require.Equal(t, claimedAt.Truncate(time.Second), firings[0].CreatedAt.Truncate(time.Second),
		"created_at still dates the first attempt, so a retry reads as a retry")

	// And the retry's own claim is exclusive again from the moment it was taken.
	claimed, err = at(runtimetypes.StaleEventFiringClaim+2*time.Second).BeginEventFiring(ctx, "on-report", 1, "evt-third")
	require.NoError(t, err)
	require.False(t, claimed, "the takeover restarts the clock; it does not inherit the dead host's age")
}

// TestUnit_EventFirings_FinishedClaimsAreNeverReclaimed pins the other half:
// only a running row is reclaimable. An outcome already recorded is final at any
// age — at-most-once is what the claim exists for.
func TestUnit_EventFirings_FinishedClaimsAreNeverReclaimed(t *testing.T) {
	ctx := context.Background()
	db := setupEventDB(t)
	now := time.Now().UTC()
	store := mustFiringStore(t, db.WithoutTransaction(), testEventWS)
	ancient := mustFiringStore(t, db.WithoutTransaction(), testEventWS,
		runtimetypes.WithEventFiringClock(func() time.Time { return now.Add(30 * 24 * time.Hour) }))

	for nid, status := range map[int64]string{
		1: runtimetypes.EventFiringStatusOK,
		2: runtimetypes.EventFiringStatusError,
		3: runtimetypes.EventFiringStatusRefused,
	} {
		seedEventFiring(t, store, "on-report", nid, status, "")
		claimed, err := ancient.BeginEventFiring(ctx, "on-report", nid, "evt-retry")
		require.NoError(t, err)
		require.False(t, claimed, "a %s firing is done, however long ago", status)
	}
}

// TestUnit_EventFirings_StrandedNamesTheInvisibleFailure pins the operator-side
// predicate: a stranded claim has neither failed nor succeeded, so a summary
// counting only error/refused cannot see it.
func TestUnit_EventFirings_StrandedNamesTheInvisibleFailure(t *testing.T) {
	now := time.Now().UTC()
	fresh := runtimetypes.EventFiring{Status: runtimetypes.EventFiringStatusRunning, UpdatedAt: now.Add(-time.Minute)}
	require.False(t, fresh.Stranded(now), "a live firing is not trouble")

	stale := runtimetypes.EventFiring{Status: runtimetypes.EventFiringStatusRunning, UpdatedAt: now.Add(-runtimetypes.StaleEventFiringClaim - time.Second)}
	require.True(t, stale.Stranded(now))

	for _, status := range []string{runtimetypes.EventFiringStatusOK, runtimetypes.EventFiringStatusError, runtimetypes.EventFiringStatusRefused} {
		done := runtimetypes.EventFiring{Status: status, UpdatedAt: now.Add(-365 * 24 * time.Hour)}
		require.False(t, done.Stranded(now), "%s is an outcome, not a strand", status)
	}
}

// TestUnit_EventFirings_NewestFirst pins the observability read's default
// order: an operator asking "what just happened" reads down, not up.
func TestUnit_EventFirings_NewestFirst(t *testing.T) {
	ctx := context.Background()
	db := setupEventDB(t)
	store := mustFiringStore(t, db.WithoutTransaction(), testEventWS)

	seedEventFiring(t, store, "on-report", 1, runtimetypes.EventFiringStatusOK, "")
	seedEventFiring(t, store, "on-report", 2, runtimetypes.EventFiringStatusError, "chain blew up")
	seedEventFiring(t, store, "on-report", 3, runtimetypes.EventFiringStatusRefused, "hop 5 exceeds limit 4")

	firings, err := store.ListEventFirings(ctx, runtimetypes.EventFiringFilter{})
	require.NoError(t, err)
	require.Equal(t, []int64{3, 2, 1}, eventFiringNIDs(firings))
	require.Equal(t, runtimetypes.EventFiringStatusRefused, firings[0].Status)
	require.Equal(t, "chain blew up", firings[1].Error)
	require.Equal(t, testEventWS, firings[0].WorkspaceID)
	require.False(t, firings[0].UpdatedAt.IsZero(), "the outcome time is what the table prints")
}

// TestUnit_EventFirings_EachFilterNarrows pins the three optional predicates
// and their combination — each one narrows, none widens.
func TestUnit_EventFirings_EachFilterNarrows(t *testing.T) {
	ctx := context.Background()
	db := setupEventDB(t)
	store := mustFiringStore(t, db.WithoutTransaction(), testEventWS)

	seedEventFiring(t, store, "on-report", 1, runtimetypes.EventFiringStatusOK, "")
	seedEventFiring(t, store, "on-status", 2, runtimetypes.EventFiringStatusError, "boom")
	seedEventFiring(t, store, "on-report", 3, runtimetypes.EventFiringStatusError, "boom again")
	seedEventFiring(t, store, "on-report", 4, runtimetypes.EventFiringStatusRefused, "hop 5 exceeds limit 4")

	t.Run("since nid", func(t *testing.T) {
		firings, err := store.ListEventFirings(ctx, runtimetypes.EventFiringFilter{SinceNID: 2})
		require.NoError(t, err)
		require.Equal(t, []int64{4, 3}, eventFiringNIDs(firings), "strictly greater than the cursor, still newest first")
	})

	t.Run("status", func(t *testing.T) {
		firings, err := store.ListEventFirings(ctx, runtimetypes.EventFiringFilter{Status: runtimetypes.EventFiringStatusError})
		require.NoError(t, err)
		require.Equal(t, []int64{3, 2}, eventFiringNIDs(firings))

		refused, err := store.ListEventFirings(ctx, runtimetypes.EventFiringFilter{Status: runtimetypes.EventFiringStatusRefused})
		require.NoError(t, err)
		require.Equal(t, []int64{4}, eventFiringNIDs(refused))
	})

	t.Run("trigger name", func(t *testing.T) {
		firings, err := store.ListEventFirings(ctx, runtimetypes.EventFiringFilter{TriggerName: "on-report"})
		require.NoError(t, err)
		require.Equal(t, []int64{4, 3, 1}, eventFiringNIDs(firings))
	})

	t.Run("combined", func(t *testing.T) {
		firings, err := store.ListEventFirings(ctx, runtimetypes.EventFiringFilter{
			SinceNID:    1,
			Status:      runtimetypes.EventFiringStatusError,
			TriggerName: "on-report",
		})
		require.NoError(t, err)
		require.Equal(t, []int64{3}, eventFiringNIDs(firings))
	})
}

// TestUnit_EventFirings_LimitDefaultsAndCeiling pins the bound: no limit means
// DefaultFiringLimit, an unbounded ask is clamped to MaxFiringLimit.
func TestUnit_EventFirings_LimitDefaultsAndCeiling(t *testing.T) {
	ctx := context.Background()
	db := setupEventDB(t)
	store := mustFiringStore(t, db.WithoutTransaction(), testEventWS)

	for nid := int64(1); nid <= runtimetypes.DefaultEventFiringLimit+5; nid++ {
		seedEventFiring(t, store, "on-report", nid, runtimetypes.EventFiringStatusOK, "")
	}

	def, err := store.ListEventFirings(ctx, runtimetypes.EventFiringFilter{})
	require.NoError(t, err)
	require.Len(t, def, runtimetypes.DefaultEventFiringLimit)
	require.Equal(t, int64(runtimetypes.DefaultEventFiringLimit+5), def[0].NID, "the default page is the newest one")

	explicit, err := store.ListEventFirings(ctx, runtimetypes.EventFiringFilter{Limit: 3})
	require.NoError(t, err)
	require.Len(t, explicit, 3)

	clamped, err := store.ListEventFirings(ctx, runtimetypes.EventFiringFilter{Limit: runtimetypes.MaxEventFiringLimit * 10})
	require.NoError(t, err)
	require.Len(t, clamped, runtimetypes.DefaultEventFiringLimit+5, "the ceiling clamps the query, it never errors")
}

// TestUnit_EventFirings_WorkspaceIsolation pins the store's construction-time
// scope on the read path too: one workspace's firings are invisible from
// another's listing, filters included.
func TestUnit_EventFirings_WorkspaceIsolation(t *testing.T) {
	ctx := context.Background()
	db := setupEventDB(t)
	storeA := mustFiringStore(t, db.WithoutTransaction(), "ws-a")
	storeB := mustFiringStore(t, db.WithoutTransaction(), "ws-b")

	// Same trigger name, same nids, same statuses in both workspaces.
	seedEventFiring(t, storeA, "iso", 1, runtimetypes.EventFiringStatusOK, "")
	seedEventFiring(t, storeA, "iso", 2, runtimetypes.EventFiringStatusError, "a failed")
	seedEventFiring(t, storeB, "iso", 1, runtimetypes.EventFiringStatusOK, "")
	seedEventFiring(t, storeB, "iso", 2, runtimetypes.EventFiringStatusError, "b failed")

	firingsA, err := storeA.ListEventFirings(ctx, runtimetypes.EventFiringFilter{})
	require.NoError(t, err)
	require.Len(t, firingsA, 2)
	for _, f := range firingsA {
		require.Equal(t, "ws-a", f.WorkspaceID)
	}

	failedA, err := storeA.ListEventFirings(ctx, runtimetypes.EventFiringFilter{Status: runtimetypes.EventFiringStatusError, TriggerName: "iso"})
	require.NoError(t, err)
	require.Len(t, failedA, 1)
	require.Equal(t, "a failed", failedA[0].Error, "a filter never reaches into the other workspace")

	empty, err := mustFiringStore(t, db.WithoutTransaction(), "ws-c").ListEventFirings(ctx, runtimetypes.EventFiringFilter{})
	require.NoError(t, err)
	require.Empty(t, empty, "a workspace that never fired sees nothing, not an error")
}

// TestUnit_EventFirings_EmptyIsNotAnError pins that no match is an answer: an
// empty, non-nil slice and no error, whatever the filter.
func TestUnit_EventFirings_EmptyIsNotAnError(t *testing.T) {
	ctx := context.Background()
	db := setupEventDB(t)
	store := mustFiringStore(t, db.WithoutTransaction(), testEventWS)

	for _, f := range []runtimetypes.EventFiringFilter{
		{},
		{SinceNID: 99},
		{Status: runtimetypes.EventFiringStatusError},
		{TriggerName: "never-authored"},
	} {
		firings, err := store.ListEventFirings(ctx, f)
		require.NoError(t, err)
		require.NotNil(t, firings)
		require.Empty(t, firings)
	}

	seedEventFiring(t, store, "on-report", 1, runtimetypes.EventFiringStatusOK, "")
	firings, err := store.ListEventFirings(ctx, runtimetypes.EventFiringFilter{TriggerName: "typo-in-listen-for"})
	require.NoError(t, err)
	require.Empty(t, firings, "a trigger that never matched records nothing — the typo's symptom")
}

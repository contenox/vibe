package hitlservice_test

import (
	"context"
	"testing"
	"time"

	"github.com/contenox/contenox/internal/kernel/taskengine"
	"github.com/contenox/contenox/internal/services/hitlservice"
	"github.com/contenox/contenox/internal/store/runtimetypes"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func recorder(t *testing.T, svc hitlservice.Service) hitlservice.ApprovalRecorder {
	t.Helper()
	rec, ok := svc.(hitlservice.ApprovalRecorder)
	require.True(t, ok)
	return rec
}

// TestUnit_ApprovalRow_WaitIsWrittenIntoExpiresAt walks the three waits an
// operator can state — a rule's own duration, no rule wait (the configured
// ceiling), and no deadline at all — and pins what each writes into the row,
// since expires_at is the only thing a restarted process can read back.
func TestUnit_ApprovalRow_WaitIsWrittenIntoExpiresAt(t *testing.T) {
	t.Parallel()
	ctx, store, _ := setupHITLDB(t)

	for _, tc := range []struct {
		name     string
		ceiling  time.Duration
		timeoutS int
		want     time.Duration // zero means: no deadline at all
	}{
		{name: "rule duration wins", ceiling: time.Minute, timeoutS: 30, want: 30 * time.Second},
		{name: "unset falls to the configured ceiling", ceiling: 90 * time.Minute, timeoutS: 0, want: 90 * time.Minute},
		{name: "unset with no configured ceiling falls to the compiled-in one", timeoutS: 0, want: hitlservice.FallbackApprovalCeiling},
		{name: "the rule says wait forever", ceiling: time.Minute, timeoutS: hitlservice.TimeoutIndefinite},
		{name: "the operator's ceiling says wait forever", ceiling: hitlservice.WaitIndefinite, timeoutS: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc := newDurableService(t, store)
			hitlservice.SetApprovalCeiling(svc, tc.ceiling)

			id := uuid.NewString()
			require.NoError(t, recorder(t, svc).RecordPendingApproval(ctx, id, hitlservice.ApprovalRequest{
				ToolsName: "local_shell", ToolName: "local_shell", TimeoutS: tc.timeoutS,
			}))

			row, err := store.GetHITLApproval(ctx, id)
			require.NoError(t, err)
			require.Equal(t, runtimetypes.HITLApprovalPending, row.State)
			if tc.want == 0 {
				require.True(t, row.ExpiresAt.IsZero(),
					"an ask nobody gave a deadline must carry no deadline, not a large one")
				require.Empty(t, row.OnTimeout,
					"nothing expires, so no surface may quote an on-timeout verdict for it")
				return
			}
			require.Equal(t, string(hitlservice.ActionDeny), row.OnTimeout)
			require.WithinDuration(t, row.CreatedAt.Add(tc.want), row.ExpiresAt, time.Minute)
		})
	}
}

// TestUnit_SweepExpired_LeavesAnAskWithNoDeadlinePendingForever is the defect
// this slice exists for: an ask raised with no deadline is still pending long
// after the hour that used to kill it, and after a restart, while the ask
// beside it that WAS given a wait resolves through its on_timeout.
func TestUnit_SweepExpired_LeavesAnAskWithNoDeadlinePendingForever(t *testing.T) {
	t.Parallel()
	ctx, store, dbPath := setupHITLDB(t)
	svc := newDurableService(t, store)

	longAgo := time.Now().UTC().Add(-72 * time.Hour)
	forever := seedPendingRow(t, ctx, store, "", longAgo, time.Time{})
	bounded := seedPendingRow(t, ctx, store, "deny", longAgo, longAgo.Add(time.Hour))

	n, err := svc.SweepExpired(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, n, "only the ask that was given a wait may expire")

	closed, err := store.GetHITLApproval(ctx, bounded.ID)
	require.NoError(t, err)
	require.Equal(t, runtimetypes.HITLApprovalExpired, closed.State)
	require.False(t, decodeApproved(t, closed.Resolution), "on_timeout deny still applies")

	open, err := store.GetHITLApproval(ctx, forever.ID)
	require.NoError(t, err)
	require.Equal(t, runtimetypes.HITLApprovalPending, open.State,
		"three days on, an ask with no deadline is still a question waiting for an answer")

	// Restart the box: the ask is still there, and still answerable.
	ctx2, store2 := reopenHITLDB(t, dbPath)
	svc2 := newDurableService(t, store2)
	n2, err := svc2.SweepExpired(ctx2)
	require.NoError(t, err)
	require.Zero(t, n2)
	require.NoError(t, svc2.Respond(ctx2, forever.ID, true))

	answered, err := store2.GetHITLApproval(ctx2, forever.ID)
	require.NoError(t, err)
	require.Equal(t, runtimetypes.HITLApprovalApproved, answered.State)
}

// TestUnit_RequestApproval_IndefiniteWaitBlocksUntilAnswered pins the waiter
// side: with no deadline there is no ceiling left to fire, so the call ends
// only when a verdict lands.
func TestUnit_RequestApproval_IndefiniteWaitBlocksUntilAnswered(t *testing.T) {
	t.Parallel()
	ctx, store, _ := setupHITLDB(t)
	svc := newDurableService(t, store)
	hitlservice.SetApprovalCeiling(svc, hitlservice.WaitIndefinite)

	published := make(chan string, 1)
	reqCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan bool, 1)
	go func() {
		approved, err := svc.RequestApproval(reqCtx, hitlservice.ApprovalRequest{
			ToolsName: "local_fs", ToolName: "write_file",
		}, signalSink{published})
		if err == nil {
			done <- approved
		}
	}()

	var id string
	select {
	case id = <-published:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the pending row")
	}

	select {
	case <-done:
		t.Fatal("an ask with no deadline resolved itself; nobody answered it")
	case <-time.After(300 * time.Millisecond):
	}

	require.NoError(t, svc.Respond(ctx, id, true))
	select {
	case approved := <-done:
		require.True(t, approved)
	case <-time.After(5 * time.Second):
		t.Fatal("the verdict never woke the waiter")
	}
}

// TestUnit_RequestAttention_HonorsAnIndefiniteCeiling pins that a mission's
// question follows the same ceiling a permission ask does.
func TestUnit_RequestAttention_HonorsAnIndefiniteCeiling(t *testing.T) {
	t.Parallel()
	ctx, store, _ := setupHITLDB(t)
	svc := newDurableService(t, store)
	hitlservice.SetApprovalCeiling(svc, hitlservice.WaitIndefinite)

	askID := uuid.NewString()
	_, err := svc.RequestAttention(ctx, hitlservice.AttentionRequest{
		AskID: askID, Summary: "which branch should I target?",
	}, taskengine.NoopTaskEventSink{})
	var pending *hitlservice.AttentionPendingError
	require.ErrorAs(t, err, &pending)

	row, err := store.GetHITLApproval(ctx, askID)
	require.NoError(t, err)
	require.True(t, row.ExpiresAt.IsZero())

	n, err := svc.SweepExpired(ctx)
	require.NoError(t, err)
	require.Zero(t, n)
}

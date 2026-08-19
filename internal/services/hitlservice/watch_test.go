package hitlservice_test

import (
	"testing"
	"time"

	"github.com/contenox/contenox/internal/services/hitlservice"
	"github.com/stretchr/testify/require"
)

// TestUnit_ApprovalVerdict_ReadsTheRowNotAChannel pins the read a blocked asker
// depends on: the durable row is where a verdict lives, whoever wrote it, and a
// pending row is not a verdict of any kind.
func TestUnit_ApprovalVerdict_ReadsTheRowNotAChannel(t *testing.T) {
	ctx, store, _ := setupHITLDB(t)
	svc := newDurableService(t, store)
	watcher, ok := svc.(hitlservice.ApprovalWatcher)
	require.True(t, ok, "every store-backed service must be readable by a waiter")

	recorder, ok := svc.(hitlservice.ApprovalRecorder)
	require.True(t, ok)
	require.NoError(t, recorder.RecordPendingApproval(ctx, "ask-open", hitlservice.ApprovalRequest{
		ToolsName: "native-git", ToolName: "git_diff", TimeoutS: 60,
	}))

	_, terminal, err := watcher.ApprovalVerdict(ctx, "ask-open")
	require.NoError(t, err)
	require.False(t, terminal, "a pending row must never read as an answer")

	// Written by somebody else entirely — a second terminal, a phone, an agent.
	require.NoError(t, svc.RespondWithGuidance(ctx, "ask-open", true, "phone", ""))

	approved, terminal, err := watcher.ApprovalVerdict(ctx, "ask-open")
	require.NoError(t, err)
	require.True(t, terminal)
	require.True(t, approved)

	_, _, err = watcher.ApprovalVerdict(ctx, "no-such-ask")
	require.Error(t, err, "an unreadable row is an error, not a silent denial")
}

// TestUnit_ApprovalVerdict_ExpiredRowCarriesItsOnTimeout pins that a swept row
// hands the waiter the verdict the envelope named, not a blanket denial.
func TestUnit_ApprovalVerdict_ExpiredRowCarriesItsOnTimeout(t *testing.T) {
	ctx, store, _ := setupHITLDB(t)
	svc := newDurableService(t, store)
	watcher := svc.(hitlservice.ApprovalWatcher)
	recorder := svc.(hitlservice.ApprovalRecorder)

	require.NoError(t, recorder.RecordPendingApproval(ctx, "ask-allow", hitlservice.ApprovalRequest{
		ToolsName: "native-git", ToolName: "git_diff",
		TimeoutS: 1, OnTimeout: hitlservice.ActionAllow,
	}))
	require.NoError(t, recorder.RecordPendingApproval(ctx, "ask-deny", hitlservice.ApprovalRequest{
		ToolsName: "native-git", ToolName: "git_diff",
		TimeoutS: 1, OnTimeout: hitlservice.ActionDeny,
	}))

	require.Eventually(t, func() bool {
		n, err := svc.SweepExpired(ctx)
		return err == nil && n == 2
	}, 5*time.Second, 100*time.Millisecond)

	approved, terminal, err := watcher.ApprovalVerdict(ctx, "ask-allow")
	require.NoError(t, err)
	require.True(t, terminal)
	require.True(t, approved, "on_timeout = allow is the verdict the run must carry on with")

	approved, terminal, err = watcher.ApprovalVerdict(ctx, "ask-deny")
	require.NoError(t, err)
	require.True(t, terminal)
	require.False(t, approved)
}

// TestUnit_RegisterApprovalWaiter_WakesBeforeThePoll pins the shortcut a blocked
// asker registers before its ask is offered, so a verdict landing immediately
// wakes it rather than waiting out a poll interval. It is only a shortcut: the
// row above stays the authority.
func TestUnit_RegisterApprovalWaiter_WakesBeforeThePoll(t *testing.T) {
	ctx, store, _ := setupHITLDB(t)
	svc := newDurableService(t, store)
	reg, ok := svc.(hitlservice.ApprovalWaiterRegistry)
	require.True(t, ok)
	recorder := svc.(hitlservice.ApprovalRecorder)

	waiter, release := reg.RegisterApprovalWaiter("ask-fast")
	defer release()
	require.NoError(t, recorder.RecordPendingApproval(ctx, "ask-fast", hitlservice.ApprovalRequest{
		ToolsName: "native-git", ToolName: "git_diff", TimeoutS: 60,
	}))
	require.NoError(t, svc.Respond(ctx, "ask-fast", true))

	select {
	case approved := <-waiter:
		require.True(t, approved)
	case <-time.After(time.Second):
		t.Fatal("a verdict recorded in this process must wake the registered waiter")
	}

	// Releasing twice is what a caller that unparks early and also defers does.
	release()
	release()
}

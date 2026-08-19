package agentservice_test

import (
	"context"
	"path/filepath"
	"sync"
	"testing"

	"github.com/contenox/contenox/internal/kernel/taskengine"
	"github.com/contenox/contenox/internal/services/agentservice"
	"github.com/contenox/contenox/internal/services/hitlservice"
	"github.com/contenox/contenox/internal/store/runtimetypes"
	libdb "github.com/contenox/contenox/libdbexec"
	"github.com/stretchr/testify/require"
)

// TestSystem_S6Gate_BlockingRunLeavingMidWaitResumesLater pins design step 5 for
// the path that is NOT detached: the run blocks on its ask, the process is taken
// away while it waits (a quit, a shutdown, a closed laptop), and what it leaves
// behind is enough. The row stays pending and the checkpoint is written on the
// way out — including onto a context that is already cancelled, which is the
// only reason there is anything to resume at all. A verdict recorded later, in
// another process, then carries the run to completion through the one hook.
func TestSystem_S6Gate_BlockingRunLeavingMidWaitResumesLater(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "s6-leaving.db")
	ctx := context.Background()
	const sessionID = "sess-leaving"

	raised := make(chan struct{})
	var once sync.Once
	holdingAsk := func(askCtx context.Context, _ hitlservice.ApprovalRequest) (bool, error) {
		once.Do(func() { close(raised) })
		<-askCtx.Done()
		return false, askCtx.Err()
	}

	a := newE2EInstance(t, dbPath, holdingAsk)
	createSession(t, a.db, sessionID)

	// Deliberately NOT detachedRun: this run waits on its ask like an attended one.
	runCtx, leave := context.WithCancel(ctx)
	defer leave()
	go func() {
		<-raised
		leave()
	}()

	resp, err := a.agent.Prompt(runCtx, agentservice.PromptRequest{
		SessionID:  sessionID,
		InputValue: e2eInput(),
		InputType:  taskengine.DataTypeChatHistory,
		Chain:      e2eChain(),
		ChainRef:   "e2e-chain.json",
	})
	require.NoError(t, err, "a process leaving mid-wait suspends; it does not fail the run")
	require.Equal(t, agentservice.StopSuspended, resp.StopReason)
	require.Equal(t, "call-w1", resp.SuspendedApprovalID)
	require.Empty(t, a.inner.execs, "the gated tool must not have run")

	row, err := a.store.GetHITLApproval(ctx, "call-w1")
	require.NoError(t, err)
	require.Equal(t, runtimetypes.HITLApprovalPending, row.State,
		"leaving is not a verdict: the ask stays answerable")
	cpRow, err := a.store.GetChainCheckpoint(ctx, "call-w1")
	require.NoError(t, err,
		"the checkpoint must be written even though the context that triggered it was cancelled")
	require.Equal(t, sessionID, cpRow.SessionID)

	a.close()

	// Another process entirely, and a verdict written there.
	b := newE2EInstance(t, dbPath, holdingAsk)
	defer b.close()
	require.NoError(t, b.hitl.Respond(ctx, "call-w1", true))

	require.Equal(t, []string{"write"}, b.inner.execs, "the gated tool runs on the resume")
	_, err = b.store.GetChainCheckpoint(ctx, "call-w1")
	require.ErrorIs(t, err, libdb.ErrNotFound, "a completed resume drops its checkpoint")
	row, err = b.store.GetHITLApproval(ctx, "call-w1")
	require.NoError(t, err)
	require.Equal(t, runtimetypes.HITLApprovalApproved, row.State)
}

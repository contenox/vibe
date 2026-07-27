package hitlservice_test

// The dual-inbox regression: a fleet-unit's gated call used to file TWO rows —
// the child wrapper's (ID = engine tool-call ID, wired to the checkpoint) and
// a parent-side twin RequestApproval minted when the ask crossed the ACP wire.
// Answering the child's worked; the twin sat pending until the sweeper expired
// it. These tests pin the fix: a caller-supplied ToolCallID is the ask's
// durable identity, adopted rather than duplicated.

import (
	"context"
	"testing"
	"time"

	"github.com/contenox/beam/internal/services/hitlservice"
	"github.com/contenox/beam/internal/store/runtimetypes"
	"github.com/stretchr/testify/require"
)

// The wire ordering that produced the wart: the child files the row first,
// then the parent's answerer calls RequestApproval with the same ToolCallID.
// Exactly ONE row must exist, and the child-side resolve (another process,
// another service instance — no shared channel) must end the parent's wait.
func TestUnit_RequestApproval_AdoptsPreFiledToolCallRow(t *testing.T) {
	ctx, storeA, dbPath := setupHITLDB(t)
	svcA := newDurableService(t, storeA)

	const callID = "call-7c1f9e4a"
	recorderA, ok := svcA.(hitlservice.ApprovalRecorder)
	require.True(t, ok, "the durable service must implement ApprovalRecorder")
	require.NoError(t, recorderA.RecordPendingApproval(ctx, callID, hitlservice.ApprovalRequest{
		ToolsName: "local_shell",
		ToolName:  "exec",
	}))

	// The parent process: a fresh instance over the same file, empty pending map.
	ctxB, storeB := reopenHITLDB(t, dbPath)
	svcB := newDurableService(t, storeB)

	published := make(chan string, 1)
	result := make(chan bool, 1)
	errs := make(chan error, 1)
	go func() {
		approved, err := svcB.RequestApproval(ctxB, hitlservice.ApprovalRequest{
			ToolCallID: callID,
			ToolsName:  "local_shell",
			ToolName:   "exec",
		}, signalSink{ids: published})
		errs <- err
		result <- approved
	}()

	select {
	case id := <-published:
		require.Equal(t, callID, id, "the adopted ask keeps the child's ID on its event")
	case <-time.After(5 * time.Second):
		t.Fatal("RequestApproval never published its event")
	}

	// One row, not two — the wart itself.
	rows, err := storeA.ListHITLApprovals(ctx, runtimetypes.HITLApprovalPending, nil, 100)
	require.NoError(t, err)
	require.Len(t, rows, 1, "adopting must not file a twin row")
	require.Equal(t, callID, rows[0].ID)

	// The CHILD resolves its own row (ResolveApprovalInline is the fast-path
	// the wrapper runs when its attached session answers) — svcB's poll on the
	// durable row must deliver that verdict to the parked parent.
	require.NoError(t, recorderA.ResolveApprovalInline(ctx, callID, true))

	select {
	case err := <-errs:
		require.NoError(t, err)
		require.True(t, <-result, "the child's approval must reach the adopted waiter")
	case <-time.After(10 * time.Second):
		t.Fatal("the adopted waiter never saw the durable resolve (poll lane broken)")
	}
}

// A row already terminal when the parent's request arrives (the operator was
// faster than the wire): the verdict returns immediately, no wait, no twin.
func TestUnit_RequestApproval_TerminalToolCallRowReturnsVerdictImmediately(t *testing.T) {
	ctx, store, _ := setupHITLDB(t)
	svc := newDurableService(t, store)

	const callID = "call-already-answered"
	recorder, ok := svc.(hitlservice.ApprovalRecorder)
	require.True(t, ok, "the durable service must implement ApprovalRecorder")
	require.NoError(t, recorder.RecordPendingApproval(ctx, callID, hitlservice.ApprovalRequest{
		ToolsName: "local_fs", ToolName: "write_file",
	}))
	require.NoError(t, svc.Respond(ctx, callID, true))

	done := make(chan bool, 1)
	go func() {
		approved, err := svc.RequestApproval(ctx, hitlservice.ApprovalRequest{
			ToolCallID: callID,
			ToolsName:  "local_fs", ToolName: "write_file",
		}, signalSink{ids: make(chan string, 1)})
		require.NoError(t, err)
		done <- approved
	}()
	select {
	case approved := <-done:
		require.True(t, approved)
	case <-time.After(2 * time.Second):
		t.Fatal("a terminal row must return its verdict without waiting")
	}

	rows, err := store.ListHITLApprovals(ctx, runtimetypes.HITLApprovalPending, nil, 100)
	require.NoError(t, err)
	require.Empty(t, rows, "no twin row may appear for an answered ask")
}

// Without a ToolCallID nothing changes: a fresh uuid row per request, exactly
// the pre-dedup behavior every existing caller relies on.
func TestUnit_RequestApproval_NoToolCallIDStillMintsFreshRow(t *testing.T) {
	ctx, store, _ := setupHITLDB(t)
	svc := newDurableService(t, store)

	published := make(chan string, 1)
	reqCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		_, _ = svc.RequestApproval(reqCtx, hitlservice.ApprovalRequest{
			ToolsName: "local_fs", ToolName: "write_file",
		}, signalSink{ids: published})
	}()
	var id string
	select {
	case id = <-published:
	case <-time.After(5 * time.Second):
		t.Fatal("RequestApproval never published its event")
	}
	require.NotEmpty(t, id)
	row, err := store.GetHITLApproval(ctx, id)
	require.NoError(t, err)
	require.Equal(t, runtimetypes.HITLApprovalPending, row.State)
	cancel()
}

package hitlservice_test

// Resume wire-in tests: Respond triggers the registered resume hook only when
// the waiter is gone, never when parked, and never from the inline resolve.

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/contenox/beam/internal/kernel/taskengine"
	"github.com/contenox/beam/internal/services/hitlservice"
	"github.com/stretchr/testify/require"
)

type hookRecorder struct {
	mu    sync.Mutex
	calls []string
	err   error
}

func (h *hookRecorder) hook(_ context.Context, approvalID string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.calls = append(h.calls, approvalID)
	return h.err
}

func (h *hookRecorder) callIDs() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.calls...)
}

func pendingRow(t *testing.T, ctx context.Context, svc hitlservice.Service, id string) {
	t.Helper()
	recorder, ok := svc.(hitlservice.ApprovalRecorder)
	require.True(t, ok, "the durable service must implement ApprovalRecorder")
	require.NoError(t, recorder.RecordPendingApproval(ctx, id, hitlservice.ApprovalRequest{
		ToolsName: "local_fs", ToolName: "write_file",
		Args: map[string]any{"path": "/tmp/x"},
	}))
}

func TestUnit_ResumeHook_FiresOnWaiterlessRespond(t *testing.T) {
	ctx, store, _ := setupHITLDB(t)
	svc := newDurableService(t, store)
	rec := &hookRecorder{err: hitlservice.ErrNoCheckpoint}
	hitlservice.SetResumeHook(svc, rec.hook)

	pendingRow(t, ctx, svc, "appr-1")
	// Nobody is parked on appr-1: Respond must run the hook.
	require.NoError(t, svc.Respond(ctx, "appr-1", true))
	require.Equal(t, []string{"appr-1"}, rec.callIDs())
}

func TestUnit_ResumeHook_ErrorSurfacesButVerdictStands(t *testing.T) {
	ctx, store, _ := setupHITLDB(t)
	svc := newDurableService(t, store)
	rec := &hookRecorder{err: errors.New("resume exploded")}
	hitlservice.SetResumeHook(svc, rec.hook)

	pendingRow(t, ctx, svc, "appr-2")
	err := svc.Respond(ctx, "appr-2", true)
	require.Error(t, err)
	require.Contains(t, err.Error(), "verdict for approval appr-2 recorded")
	require.Contains(t, err.Error(), "resume exploded")

	// The verdict IS durable: a second Respond reports already-answered.
	require.ErrorIs(t, svc.Respond(ctx, "appr-2", true), hitlservice.ErrApprovalAlreadyResolved)
}

func TestUnit_ResumeHook_NotFiredWhileWaiterParked(t *testing.T) {
	ctx, store, _ := setupHITLDB(t)
	svc := newDurableService(t, store)
	rec := &hookRecorder{}
	hitlservice.SetResumeHook(svc, rec.hook)

	type result struct {
		approved bool
		err      error
	}
	done := make(chan result, 1)
	started := make(chan string, 1)
	go func() {
		approved, err := svc.RequestApproval(ctx, hitlservice.ApprovalRequest{
			ToolsName: "local_fs", ToolName: "write_file",
		}, sinkCapturingApprovalID(started))
		done <- result{approved, err}
	}()

	approvalID := <-started
	require.NoError(t, svc.Respond(ctx, approvalID, true))
	res := <-done
	require.NoError(t, res.err)
	require.True(t, res.approved, "the parked waiter got the verdict")
	require.Empty(t, rec.callIDs(), "an in-process waiter IS the resume; the hook must not double-run")
}

func TestUnit_ResumeHook_NotFiredByInlineResolve(t *testing.T) {
	ctx, store, _ := setupHITLDB(t)
	svc := newDurableService(t, store)
	rec := &hookRecorder{}
	hitlservice.SetResumeHook(svc, rec.hook)

	pendingRow(t, ctx, svc, "appr-3")
	recorder := svc.(hitlservice.ApprovalRecorder)
	require.NoError(t, recorder.ResolveApprovalInline(ctx, "appr-3", false))
	require.Empty(t, rec.callIDs(), "the wrapper resolving its own fast-path verdict is the waiter itself")
}

func TestUnit_ResumeHook_SweepExpiredResumesWithTimeoutOutcome(t *testing.T) {
	ctx, store, _ := setupHITLDB(t)
	svc := newDurableService(t, store)
	rec := &hookRecorder{err: hitlservice.ErrNoCheckpoint}
	hitlservice.SetResumeHook(svc, rec.hook)

	recorder := svc.(hitlservice.ApprovalRecorder)
	// TimeoutS 1 puts expiry ~1s out, keeping this test fast.
	require.NoError(t, recorder.RecordPendingApproval(ctx, "appr-exp", hitlservice.ApprovalRequest{
		ToolsName: "local_fs", ToolName: "write_file", TimeoutS: 1,
	}))
	time.Sleep(1100 * time.Millisecond)

	n, err := svc.SweepExpired(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, n)
	require.Equal(t, []string{"appr-exp"}, rec.callIDs(),
		"an expired approval backing a suspended run must resume with the timeout outcome, not strand its checkpoint")
}

// sinkCapturingApprovalID forwards the approval_requested event's ID to ch —
// how the waiter-parked test learns the RequestApproval-minted ID.
func sinkCapturingApprovalID(ch chan<- string) taskengine.TaskEventSink {
	return &approvalIDSink{ch: ch}
}

type approvalIDSink struct{ ch chan<- string }

func (s *approvalIDSink) PublishTaskEvent(_ context.Context, ev taskengine.TaskEvent) error {
	if ev.Kind == taskengine.TaskEventApprovalRequested && ev.ApprovalID != "" {
		select {
		case s.ch <- ev.ApprovalID:
		default:
		}
	}
	return nil
}

func (s *approvalIDSink) Wants(taskengine.TaskEventKind) bool { return true }

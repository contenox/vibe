package hitlservice_test

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/contenox/beam/internal/kernel/taskengine"
	libdb "github.com/contenox/beam/internal/libdbexec"
	"github.com/contenox/beam/internal/libtracker"
	"github.com/contenox/beam/internal/services/hitlservice"
	"github.com/contenox/beam/internal/store/runtimetypes"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// setupHITLDB opens a fresh SQLite-backed Store at a temp path. The path
// lets reopenHITLDB simulate a `contenox serve` restart.
func setupHITLDB(t *testing.T) (context.Context, runtimetypes.Store, string) {
	t.Helper()
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "hitl_approvals.db")
	db, err := libdb.NewSQLiteDBManager(ctx, dbPath, runtimetypes.SchemaSQLite)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	return ctx, runtimetypes.New(db.WithoutTransaction()), dbPath
}

// reopenHITLDB opens a fresh DBManager/Store over the same on-disk file; no
// in-memory state (including a service's pending map) survives.
func reopenHITLDB(t *testing.T, dbPath string) (context.Context, runtimetypes.Store) {
	t.Helper()
	ctx := context.Background()
	db, err := libdb.NewSQLiteDBManager(ctx, dbPath, runtimetypes.SchemaSQLite)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	return ctx, runtimetypes.New(db.WithoutTransaction())
}

// newDurableService builds a Service over a real Store so
// RequestApproval/Respond/SweepExpired are durable.
func newDurableService(t *testing.T, store runtimetypes.Store) hitlservice.Service {
	t.Helper()
	return hitlservice.NewWithDefaultPolicy(hitlservice.NewFSPolicySource(t.TempDir()), testTenant, store, libtracker.NoopTracker{}, "")
}

// signalSink forwards each event's ApprovalID to a channel — a
// synchronization point since the row is committed before publish.
type signalSink struct {
	ids chan<- string
}

func (s signalSink) Wants(taskengine.TaskEventKind) bool { return true }

func (s signalSink) PublishTaskEvent(_ context.Context, ev taskengine.TaskEvent) error {
	s.ids <- ev.ApprovalID
	return nil
}

func decodeApproved(t *testing.T, raw json.RawMessage) bool {
	t.Helper()
	var res struct {
		Approved *bool `json:"approved"`
	}
	require.NoError(t, json.Unmarshal(raw, &res))
	require.NotNil(t, res.Approved, "resolution must carry an approved answer")
	return *res.Approved
}

func seedPendingRow(t *testing.T, ctx context.Context, store runtimetypes.Store, onTimeout string, createdAt, expiresAt time.Time) *runtimetypes.HITLApproval {
	t.Helper()
	row := &runtimetypes.HITLApproval{
		ID:        uuid.NewString(),
		ToolsName: "local_fs",
		ToolName:  "write_file",
		OnTimeout: onTimeout,
		State:     runtimetypes.HITLApprovalPending,
		CreatedAt: createdAt,
		ExpiresAt: expiresAt,
	}
	require.NoError(t, store.CreateHITLApproval(ctx, row))
	return row
}

// TestUnit_RequestApproval_SurvivesRestartAndIsAnswerableAfterward pins that
// a pending approval survives a process restart and is still answerable.
func TestUnit_RequestApproval_SurvivesRestartAndIsAnswerableAfterward(t *testing.T) {
	ctx1, store1, dbPath := setupHITLDB(t)
	svc1 := newDurableService(t, store1)

	published := make(chan string, 1)
	reqCtx, cancel := context.WithCancel(ctx1)
	result := make(chan error, 1)
	go func() {
		_, err := svc1.RequestApproval(reqCtx, hitlservice.ApprovalRequest{
			ToolsName:  "local_fs",
			ToolName:   "write_file",
			PolicyName: "hitl-policy-default.json",
		}, signalSink{published})
		result <- err
	}()

	var approvalID string
	select {
	case approvalID = <-published:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the pending row to be published")
	}
	require.NotEmpty(t, approvalID)

	pending, err := store1.GetHITLApproval(ctx1, approvalID)
	require.NoError(t, err)
	require.Equal(t, runtimetypes.HITLApprovalPending, pending.State)

	// Simulate the old process exiting mid-ask before anyone answers.
	cancel()
	select {
	case err := <-result:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(5 * time.Second):
		t.Fatal("RequestApproval did not return after its context was cancelled")
	}

	// "Restart": a fresh DBManager/Store/Service over the same on-disk file.
	ctx2, store2 := reopenHITLDB(t, dbPath)
	svc2 := newDurableService(t, store2)

	stillPending, err := store2.GetHITLApproval(ctx2, approvalID)
	require.NoError(t, err, "the pending row must survive the restart, not be lost")
	require.Equal(t, runtimetypes.HITLApprovalPending, stillPending.State)
	require.Equal(t, "local_fs", stillPending.ToolsName)
	require.Equal(t, "write_file", stillPending.ToolName)
	require.Equal(t, "hitl-policy-default.json", stillPending.PolicyName)

	require.NoError(t, svc2.Respond(ctx2, approvalID, false))
	resolved, err := store2.GetHITLApproval(ctx2, approvalID)
	require.NoError(t, err)
	require.Equal(t, runtimetypes.HITLApprovalDenied, resolved.State)
	require.False(t, decodeApproved(t, resolved.Resolution))
	require.NotNil(t, resolved.ResolvedAt)
}

// TestUnit_Respond_RecordsAnswerWhenNoRequesterIsParked pins that Respond
// durably records the answer even when nobody is parked on the row.
func TestUnit_Respond_RecordsAnswerWhenNoRequesterIsParked(t *testing.T) {
	t.Parallel()
	ctx, store, _ := setupHITLDB(t)
	svc := newDurableService(t, store)

	now := time.Now().UTC()
	row := seedPendingRow(t, ctx, store, "deny", now, now.Add(time.Hour))

	require.NoError(t, svc.Respond(ctx, row.ID, true))

	got, err := store.GetHITLApproval(ctx, row.ID)
	require.NoError(t, err)
	require.Equal(t, runtimetypes.HITLApprovalApproved, got.State)
	require.NotNil(t, got.ResolvedAt)
	require.True(t, decodeApproved(t, got.Resolution))
}

func TestUnit_Respond_UnknownIDReturnsErrApprovalNotFound(t *testing.T) {
	t.Parallel()
	ctx, store, _ := setupHITLDB(t)
	svc := newDurableService(t, store)

	err := svc.Respond(ctx, "no-such-id", true)
	require.ErrorIs(t, err, hitlservice.ErrApprovalNotFound)
}

func TestUnit_Respond_AlreadyAnsweredReturnsErrApprovalAlreadyResolved(t *testing.T) {
	t.Parallel()
	ctx, store, _ := setupHITLDB(t)
	svc := newDurableService(t, store)

	now := time.Now().UTC()
	row := seedPendingRow(t, ctx, store, "deny", now, now.Add(time.Hour))
	require.NoError(t, svc.Respond(ctx, row.ID, true))

	err := svc.Respond(ctx, row.ID, false)
	require.ErrorIs(t, err, hitlservice.ErrApprovalAlreadyResolved)

	// The first answer must stand; a second Respond must not overwrite it.
	got, getErr := store.GetHITLApproval(ctx, row.ID)
	require.NoError(t, getErr)
	require.True(t, decodeApproved(t, got.Resolution))
}

func TestUnit_Respond_ExpiredReturnsErrApprovalExpired(t *testing.T) {
	t.Parallel()
	ctx, store, _ := setupHITLDB(t)
	svc := newDurableService(t, store)

	now := time.Now().UTC()
	row := seedPendingRow(t, ctx, store, "deny", now.Add(-time.Hour), now.Add(-time.Minute))

	n, err := svc.SweepExpired(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, n)

	err = svc.Respond(ctx, row.ID, true)
	require.ErrorIs(t, err, hitlservice.ErrApprovalExpired)
}

// TestUnit_SweepExpired_AppliesOnTimeout pins that a row past its deadline
// resolves per its OnTimeout, and one before its deadline is untouched.
func TestUnit_SweepExpired_AppliesOnTimeout(t *testing.T) {
	t.Parallel()
	ctx, store, _ := setupHITLDB(t)
	svc := newDurableService(t, store)

	now := time.Now().UTC()
	expiredDeny := seedPendingRow(t, ctx, store, "deny", now.Add(-time.Hour), now.Add(-time.Minute))
	expiredDefault := seedPendingRow(t, ctx, store, "", now.Add(-time.Hour), now.Add(-time.Minute))
	notYetExpired := seedPendingRow(t, ctx, store, "deny", now, now.Add(time.Hour))

	n, err := svc.SweepExpired(ctx)
	require.NoError(t, err)
	require.Equal(t, 2, n)

	for _, id := range []string{expiredDeny.ID, expiredDefault.ID} {
		got, err := store.GetHITLApproval(ctx, id)
		require.NoError(t, err)
		require.Equal(t, runtimetypes.HITLApprovalExpired, got.State)
		require.NotNil(t, got.ResolvedAt)
		require.False(t, decodeApproved(t, got.Resolution), "default/deny on_timeout must resolve to a denial")
	}

	untouched, err := store.GetHITLApproval(ctx, notYetExpired.ID)
	require.NoError(t, err)
	require.Equal(t, runtimetypes.HITLApprovalPending, untouched.State, "a row before its deadline must be left alone")

	// Idempotent: a second sweep with nothing new to expire does nothing.
	n2, err := svc.SweepExpired(ctx)
	require.NoError(t, err)
	require.Equal(t, 0, n2)
}

// TestUnit_SweepExpired_DoesNotOverwriteAnAnswerRespondAlreadyRecorded pins
// the CAS guard: a human's answer stands even after the deadline passes.
func TestUnit_SweepExpired_DoesNotOverwriteAnAnswerRespondAlreadyRecorded(t *testing.T) {
	t.Parallel()
	ctx, store, _ := setupHITLDB(t)
	svc := newDurableService(t, store)

	now := time.Now().UTC()
	row := seedPendingRow(t, ctx, store, "deny", now.Add(-time.Hour), now.Add(-time.Minute))

	require.NoError(t, svc.Respond(ctx, row.ID, true))

	n, err := svc.SweepExpired(ctx)
	require.NoError(t, err)
	require.Equal(t, 0, n, "the row is no longer pending; there is nothing left to sweep")

	got, err := store.GetHITLApproval(ctx, row.ID)
	require.NoError(t, err)
	require.Equal(t, runtimetypes.HITLApprovalApproved, got.State, "the human's answer must stand")
	require.True(t, decodeApproved(t, got.Resolution))
}

// TestUnit_RequestApproval_BoundedWaitTerminatesWithoutARuleTimeout pins that
// TimeoutS==0 is bounded by the serve ceiling, not blocked forever.
func TestUnit_RequestApproval_BoundedWaitTerminatesWithoutARuleTimeout(t *testing.T) {
	t.Parallel()
	ctx, store, _ := setupHITLDB(t)
	svc := newDurableService(t, store)
	hitlservice.SetApprovalCeiling(svc, 50*time.Millisecond)

	type outcome struct {
		approved bool
		err      error
	}
	done := make(chan outcome, 1)
	start := time.Now()
	go func() {
		approved, err := svc.RequestApproval(ctx, hitlservice.ApprovalRequest{
			ToolsName: "local_shell",
			ToolName:  "local_shell",
			TimeoutS:  0, // no rule timeout
		}, taskengine.NoopTaskEventSink{})
		done <- outcome{approved, err}
	}()

	select {
	case res := <-done:
		require.NoError(t, res.err, "a ceiling timeout must resolve as a clean denial, not surface an error")
		require.False(t, res.approved, "a late denial beats an eternal block")
	case <-time.After(2 * time.Second):
		t.Fatal("RequestApproval hung past its serve-level ceiling — this is defect D1")
	}
	require.Less(t, time.Since(start), 2*time.Second)
}

// TestUnit_RequestApproval_RuleTimeoutWinsOverCeiling pins that a rule's own
// TimeoutS wins over a much larger serve ceiling.
func TestUnit_RequestApproval_RuleTimeoutWinsOverCeiling(t *testing.T) {
	t.Parallel()
	ctx, store, _ := setupHITLDB(t)
	svc := newDurableService(t, store)
	hitlservice.SetApprovalCeiling(svc, time.Hour) // must not be what fires below

	ruleCtx, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancel()

	start := time.Now()
	approved, err := svc.RequestApproval(ruleCtx, hitlservice.ApprovalRequest{
		ToolsName: "local_shell",
		ToolName:  "local_shell",
		TimeoutS:  1, // a rule timeout is set
	}, taskengine.NoopTaskEventSink{})
	elapsed := time.Since(start)

	require.False(t, approved)
	require.ErrorIs(t, err, context.DeadlineExceeded, "the rule-timeout branch must keep returning ctx.Err(), unchanged")
	require.Less(t, elapsed, 2*time.Second)
}

// TestUnit_RequestApproval_ExpiresAtReflectsCeilingWhenNoRuleTimeout pins
// that expires_at is created_at + the serve ceiling with no rule timeout.
func TestUnit_RequestApproval_ExpiresAtReflectsCeilingWhenNoRuleTimeout(t *testing.T) {
	t.Parallel()
	ctx, store, _ := setupHITLDB(t)
	svc := newDurableService(t, store)
	hitlservice.SetApprovalCeiling(svc, time.Minute)

	published := make(chan string, 1)
	reqCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		_, _ = svc.RequestApproval(reqCtx, hitlservice.ApprovalRequest{
			ToolsName: "local_fs", ToolName: "write_file",
		}, signalSink{published})
	}()

	var id string
	select {
	case id = <-published:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the pending row to be published")
	}

	row, err := store.GetHITLApproval(ctx, id)
	require.NoError(t, err)
	gotWindow := row.ExpiresAt.Sub(row.CreatedAt)
	require.InDelta(t, time.Minute.Seconds(), gotWindow.Seconds(), 5, "expires_at must be created_at + the serve ceiling")
}

// TestUnit_RequestApproval_Respond_ConcurrentRoundTrips exercises many
// concurrent RequestApproval/Respond round-trips under `go test -race`.
func TestUnit_RequestApproval_Respond_ConcurrentRoundTrips(t *testing.T) {
	ctx, store, _ := setupHITLDB(t)
	svc := newDurableService(t, store)

	const n = 40
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			want := i%2 == 0
			published := make(chan string, 1)

			type outcome struct {
				approved bool
				err      error
			}
			resultCh := make(chan outcome, 1)
			go func() {
				approved, err := svc.RequestApproval(ctx, hitlservice.ApprovalRequest{
					ToolsName: "local_fs",
					ToolName:  "write_file",
				}, signalSink{published})
				resultCh <- outcome{approved, err}
			}()

			var id string
			select {
			case id = <-published:
			case <-time.After(5 * time.Second):
				errs <- fmt.Errorf("round %d: timed out waiting for publish", i)
				return
			}

			if err := svc.Respond(ctx, id, want); err != nil {
				errs <- fmt.Errorf("round %d: Respond: %w", i, err)
				return
			}

			select {
			case res := <-resultCh:
				if res.err != nil {
					errs <- fmt.Errorf("round %d: RequestApproval: %w", i, res.err)
					return
				}
				if res.approved != want {
					errs <- fmt.Errorf("round %d: got approved=%v, want %v", i, res.approved, want)
				}
			case <-time.After(5 * time.Second):
				errs <- fmt.Errorf("round %d: RequestApproval never returned after Respond", i)
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

// TestUnit_RequestApproval_PersistsAttribution pins that RequestApproval
// persists the caller's attribution fields onto the durable row.
func TestUnit_RequestApproval_PersistsAttribution(t *testing.T) {
	ctx, store, _ := setupHITLDB(t)
	svc := newDurableService(t, store)

	published := make(chan string, 1)
	reqCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		_, _ = svc.RequestApproval(reqCtx, hitlservice.ApprovalRequest{
			ToolsName:  "local_fs",
			ToolName:   "write_file",
			PolicyName: "envelope.json",
			InstanceID: "instance-1",
			SessionID:  "session-1",
			AgentName:  "reviewer",
			MissionID:  "mission-1",
		}, signalSink{published})
	}()

	var approvalID string
	select {
	case approvalID = <-published:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the pending row to be published")
	}

	row, err := store.GetHITLApproval(ctx, approvalID)
	require.NoError(t, err)
	require.Equal(t, "instance-1", row.InstanceID)
	require.Equal(t, "session-1", row.SessionID)
	require.Equal(t, "reviewer", row.AgentName)
	require.NotNil(t, row.MissionID)
	require.Equal(t, "mission-1", *row.MissionID)
}

func TestUnit_RequestApproval_NoMissionStoresNull(t *testing.T) {
	ctx, store, _ := setupHITLDB(t)
	svc := newDurableService(t, store)

	published := make(chan string, 1)
	reqCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		_, _ = svc.RequestApproval(reqCtx, hitlservice.ApprovalRequest{
			ToolsName:  "local_fs",
			ToolName:   "write_file",
			InstanceID: "instance-1",
		}, signalSink{published})
	}()

	var approvalID string
	select {
	case approvalID = <-published:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the pending row to be published")
	}

	row, err := store.GetHITLApproval(ctx, approvalID)
	require.NoError(t, err)
	require.Equal(t, "instance-1", row.InstanceID)
	require.Nil(t, row.MissionID, "an ask with no mission must store NULL, not an empty string")
	require.Empty(t, row.AgentName)
}

// TestUnit_PolicyNameFromContext_RoundTrips pins that WithPolicyName and
// PolicyNameFromContext round-trip, trimmed, with a blank name pinning nothing.
func TestUnit_PolicyNameFromContext_RoundTrips(t *testing.T) {
	require.Empty(t, hitlservice.PolicyNameFromContext(context.Background()))

	ctx := hitlservice.WithPolicyName(context.Background(), "  envelope.json  ")
	require.Equal(t, "envelope.json", hitlservice.PolicyNameFromContext(ctx))

	unchanged := hitlservice.WithPolicyName(context.Background(), "   ")
	require.Empty(t, hitlservice.PolicyNameFromContext(unchanged), "a blank name pins nothing")
}

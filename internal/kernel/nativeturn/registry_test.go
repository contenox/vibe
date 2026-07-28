package nativeturn

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/contenox/contenox/libacp"
)

// recViewer is a thread-safe Viewer that records every event it is delivered, so a
// test can assert ordering, exactly-once replay, and post-detach survival.
type recViewer struct {
	id  string
	mu  sync.Mutex
	got []Event
}

func newRecViewer(id string) *recViewer { return &recViewer{id: id} }

func (v *recViewer) ID() string { return v.id }

func (v *recViewer) Deliver(_ context.Context, ev Event) error {
	v.mu.Lock()
	v.got = append(v.got, ev)
	v.mu.Unlock()
	return nil
}

func (v *recViewer) count() int {
	v.mu.Lock()
	defer v.mu.Unlock()
	return len(v.got)
}

func (v *recViewer) seqs() []uint64 {
	v.mu.Lock()
	defer v.mu.Unlock()
	out := make([]uint64, len(v.got))
	for i, ev := range v.got {
		out[i] = ev.Seq
	}
	return out
}

func (v *recViewer) texts() []string {
	v.mu.Lock()
	defer v.mu.Unlock()
	out := make([]string, len(v.got))
	for i, ev := range v.got {
		if c := ev.Update.Update.Content; c != nil {
			out[i] = c.Text
		}
	}
	return out
}

// note builds a distinct agent-message-chunk notification for sid.
func note(sid libacp.SessionID, text string) libacp.SessionNotification {
	return libacp.SessionNotification{SessionID: sid, Update: libacp.NewAgentMessageChunk(text)}
}

func eventually(t *testing.T, d time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("condition not met within %s", d)
}

func equalUint64(a, b []uint64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestNew_DefaultsFloor verifies non-positive config fields are floored to defaults.
func TestNew_DefaultsFloor(t *testing.T) {
	reg := New(Config{})
	defer reg.Close()
	if reg.cfg.TurnDeadline != DefaultTurnDeadline {
		t.Fatalf("TurnDeadline = %s, want default %s", reg.cfg.TurnDeadline, DefaultTurnDeadline)
	}
	if reg.cfg.GraceWindow != DefaultGraceWindow {
		t.Fatalf("GraceWindow = %s, want default %s", reg.cfg.GraceWindow, DefaultGraceWindow)
	}
	if reg.cfg.JournalSize != DefaultJournalSize {
		t.Fatalf("JournalSize = %d, want default %d", reg.cfg.JournalSize, DefaultJournalSize)
	}
}

// TestTurnSurvivesViewerDetach pins that a viewer detaching (a dropped
// connection) does not cancel the turn: it keeps running and completes,
// with its events captured for a still-attached viewer.
func TestTurnSurvivesViewerDetach(t *testing.T) {
	const sid = libacp.SessionID("s1")
	reg := New(Config{TurnDeadline: 5 * time.Second, GraceWindow: 5 * time.Second, JournalSize: 16})
	defer reg.Close()

	gate := make(chan struct{})
	fn := func(ctx context.Context, emit func(context.Context, libacp.SessionNotification)) Result {
		emit(ctx, note(sid, "e1"))
		<-gate // wait until the test has detached the dropped viewer
		emit(ctx, note(sid, "e2"))
		return Result{StopReason: libacp.StopReasonEndTurn}
	}

	v1 := newRecViewer("v1")
	turn, started, err := reg.Start(sid, fn, v1)
	if err != nil || !started {
		t.Fatalf("Start: started=%v err=%v", started, err)
	}
	// A second viewer stays attached across the whole turn, proving the turn keeps
	// producing after v1 leaves.
	survivor := newRecViewer("survivor")
	if _, ok, err := reg.AttachIfRunning(context.Background(), sid, survivor); err != nil || !ok {
		t.Fatalf("AttachIfRunning survivor: ok=%v err=%v", ok, err)
	}

	eventually(t, 2*time.Second, func() bool { return v1.count() == 1 && survivor.count() == 1 })

	// Simulate the connection drop: detach v1. This must not cancel the turn.
	turn.Detach(v1.ID())

	close(gate) // let the turn produce its second event and finish

	select {
	case <-turn.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("turn did not complete after viewer detach")
	}
	if got := turn.Result().StopReason; got != libacp.StopReasonEndTurn {
		t.Fatalf("Result.StopReason = %q, want end_turn", got)
	}
	// The survivor saw both events: the turn kept running and journaling
	// after the drop.
	if got := survivor.texts(); len(got) != 2 || got[0] != "e1" || got[1] != "e2" {
		t.Fatalf("survivor events = %v, want [e1 e2]", got)
	}
	// v1 only ever saw the pre-detach event.
	if got := v1.count(); got != 1 {
		t.Fatalf("v1 count = %d, want 1 (nothing after detach)", got)
	}
}

// TestReattachReplaysThenLive verifies a reconnecting viewer replays the journal
// (ordered, exactly-once) and then resumes the live stream.
func TestReattachReplaysThenLive(t *testing.T) {
	const sid = libacp.SessionID("s2")
	reg := New(Config{TurnDeadline: 5 * time.Second, GraceWindow: 5 * time.Second, JournalSize: 16})
	defer reg.Close()

	gate := make(chan struct{})
	fn := func(ctx context.Context, emit func(context.Context, libacp.SessionNotification)) Result {
		emit(ctx, note(sid, "e1"))
		emit(ctx, note(sid, "e2"))
		<-gate
		emit(ctx, note(sid, "e3"))
		return Result{StopReason: libacp.StopReasonEndTurn}
	}

	v1 := newRecViewer("v1")
	turn, _, err := reg.Start(sid, fn, v1)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	eventually(t, 2*time.Second, func() bool { return v1.count() == 2 })

	turn.Detach(v1.ID()) // drop

	v2 := newRecViewer("v2")
	if _, ok, err := reg.AttachIfRunning(context.Background(), sid, v2); err != nil || !ok {
		t.Fatalf("AttachIfRunning v2: ok=%v err=%v", ok, err)
	}
	// Replay under the lock happened synchronously in AttachIfRunning: v2 already has
	// the two journaled events, in order, before any live event.
	if got := v2.texts(); len(got) != 2 || got[0] != "e1" || got[1] != "e2" {
		t.Fatalf("v2 replay = %v, want [e1 e2]", got)
	}

	close(gate)
	eventually(t, 2*time.Second, func() bool { return v2.count() == 3 })
	<-turn.Done()

	if got := v2.texts(); len(got) != 3 || got[0] != "e1" || got[1] != "e2" || got[2] != "e3" {
		t.Fatalf("v2 events = %v, want [e1 e2 e3] (exactly-once, ordered)", got)
	}
	// Monotonic, gapless sequence numbers across replay + live.
	if got := v2.seqs(); !equalUint64(got, []uint64{1, 2, 3}) {
		t.Fatalf("v2 seqs = %v, want [1 2 3]", got)
	}
}

// TestGraceReapFires verifies Belt 1: with no viewer attached and no reattach within
// the grace window, the turn is cancelled and torn down.
func TestGraceReapFires(t *testing.T) {
	const sid = libacp.SessionID("s3")
	reg := New(Config{TurnDeadline: 5 * time.Second, GraceWindow: 80 * time.Millisecond, JournalSize: 16})
	defer reg.Close()

	var sawCancel atomic.Bool
	fn := func(ctx context.Context, emit func(context.Context, libacp.SessionNotification)) Result {
		<-ctx.Done() // block until a belt cancels the turn
		if errors.Is(ctx.Err(), context.Canceled) {
			sawCancel.Store(true)
		}
		return Result{StopReason: libacp.StopReasonCancelled}
	}

	v1 := newRecViewer("v1")
	turn, _, err := reg.Start(sid, fn, v1)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	turn.Detach(v1.ID()) // last viewer leaves; grace timer starts

	select {
	case <-turn.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("grace timer did not cancel the unwatched turn")
	}
	if !sawCancel.Load() {
		t.Fatal("turn context was not cancelled by the grace timer")
	}
	if turn.Result().StopReason != libacp.StopReasonCancelled {
		t.Fatalf("Result.StopReason = %q, want cancelled", turn.Result().StopReason)
	}
	if _, ok := reg.Get(sid); ok {
		t.Fatal("session should be torn down after grace reap")
	}
}

// TestReattachCancelsGrace verifies a reattach inside the grace window keeps the
// turn alive (Belt 1's escape hatch).
func TestReattachCancelsGrace(t *testing.T) {
	const sid = libacp.SessionID("s4")
	reg := New(Config{TurnDeadline: 5 * time.Second, GraceWindow: 150 * time.Millisecond, JournalSize: 16})
	defer reg.Close()

	fn := func(ctx context.Context, emit func(context.Context, libacp.SessionNotification)) Result {
		<-ctx.Done()
		return Result{StopReason: libacp.StopReasonCancelled}
	}

	v1 := newRecViewer("v1")
	turn, _, err := reg.Start(sid, fn, v1)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	turn.Detach(v1.ID()) // grace starts

	// Reattach well inside the window.
	v2 := newRecViewer("v2")
	if _, ok, err := reg.AttachIfRunning(context.Background(), sid, v2); err != nil || !ok {
		t.Fatalf("AttachIfRunning v2: ok=%v err=%v", ok, err)
	}

	// Wait past the original grace window; the turn must still be running.
	time.Sleep(300 * time.Millisecond)
	select {
	case <-turn.Done():
		t.Fatal("turn was reaped despite a reattach within the grace window")
	default:
	}
	st, ok := reg.Get(sid)
	if !ok {
		t.Fatal("turn should still be active after reattach")
	}
	if st.State != StateRunning {
		t.Fatalf("state = %q, want running after reattach", st.State)
	}
	turn.Cancel() // cleanup
}

// TestHardDeadlineBoundsRunaway verifies Belt 2: even with a viewer attached the
// whole time (so grace never starts), the hard deadline terminates a runaway turn.
func TestHardDeadlineBoundsRunaway(t *testing.T) {
	const sid = libacp.SessionID("s5")
	reg := New(Config{TurnDeadline: 100 * time.Millisecond, GraceWindow: 5 * time.Second, JournalSize: 16})
	defer reg.Close()

	var ctxErr atomic.Value
	fn := func(ctx context.Context, emit func(context.Context, libacp.SessionNotification)) Result {
		<-ctx.Done() // never returns on its own
		ctxErr.Store(ctx.Err())
		return Result{StopReason: libacp.StopReasonEndTurn, Err: ctx.Err()}
	}

	v1 := newRecViewer("v1") // stays attached: only the deadline can end this
	turn, _, err := reg.Start(sid, fn, v1)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	select {
	case <-turn.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("hard deadline did not bound the runaway turn")
	}
	if got, _ := ctxErr.Load().(error); !errors.Is(got, context.DeadlineExceeded) {
		t.Fatalf("turn ctx error = %v, want DeadlineExceeded", got)
	}
}

// TestSessionCancelCancelsTurn verifies session/cancel (Registry.Cancel) ends the
// turn and unblocks a viewer awaiting completion.
func TestSessionCancelCancelsTurn(t *testing.T) {
	const sid = libacp.SessionID("s6")
	reg := New(Config{TurnDeadline: 5 * time.Second, GraceWindow: 5 * time.Second, JournalSize: 16})
	defer reg.Close()

	fn := func(ctx context.Context, emit func(context.Context, libacp.SessionNotification)) Result {
		<-ctx.Done()
		return Result{StopReason: libacp.StopReasonCancelled}
	}
	v1 := newRecViewer("v1")
	turn, _, err := reg.Start(sid, fn, v1)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	// A viewer awaiting completion in the background must unblock on cancel.
	awaited := make(chan Result, 1)
	go func() {
		res, _ := turn.Await(context.Background())
		awaited <- res
	}()

	if !reg.Cancel(sid) {
		t.Fatal("Cancel returned false for a live turn")
	}
	select {
	case res := <-awaited:
		if res.StopReason != libacp.StopReasonCancelled {
			t.Fatalf("awaited StopReason = %q, want cancelled", res.StopReason)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("awaiting viewer did not unblock on cancel")
	}
	if _, ok := reg.Get(sid); ok {
		t.Fatal("session should be torn down after cancel")
	}
	// A second cancel is a clean no-op.
	if reg.Cancel(sid) {
		t.Fatal("second Cancel should return false")
	}
}

// TestConcurrentViewersReaperNeverReapsBusy runs many viewers churning against one
// live, watched turn while the reaper sweeps — the turn must never be reaped, and
// List/Get must reflect the live state. Exercises the concurrency invariants under
// -race.
func TestConcurrentViewersReaperNeverReapsBusy(t *testing.T) {
	const sid = libacp.SessionID("s7")
	reg := New(Config{TurnDeadline: 5 * time.Second, GraceWindow: 200 * time.Millisecond, JournalSize: 32})
	defer reg.Close()

	stop := make(chan struct{})
	var emitted atomic.Uint64
	fn := func(ctx context.Context, emit func(context.Context, libacp.SessionNotification)) Result {
		for {
			select {
			case <-ctx.Done():
				return Result{StopReason: libacp.StopReasonCancelled}
			case <-stop:
				return Result{StopReason: libacp.StopReasonEndTurn}
			case <-time.After(3 * time.Millisecond):
				emit(ctx, note(sid, "tick"))
				emitted.Add(1)
			}
		}
	}

	anchor := newRecViewer("anchor") // never detaches: the turn is always watched
	turn, _, err := reg.Start(sid, fn, anchor)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	var wg sync.WaitGroup
	// Churn: attach/detach transient viewers.
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			for j := 0; j < 25; j++ {
				v := newRecViewer(fmt.Sprintf("v-%d-%d", n, j))
				if h, ok, _ := reg.AttachIfRunning(context.Background(), sid, v); ok {
					time.Sleep(time.Millisecond)
					h.Detach(v.ID())
				}
			}
		}(i)
	}
	// Concurrent reaper sweeps and status reads.
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 40; j++ {
				_ = reg.ReapIdle(context.Background())
				_ = reg.List()
				_, _ = reg.Get(sid)
				time.Sleep(time.Millisecond)
			}
		}()
	}
	wg.Wait()

	// The anchored, busy turn was never reaped.
	select {
	case <-turn.Done():
		t.Fatal("busy watched turn was reaped/cancelled during the churn")
	default:
	}
	st, ok := reg.Get(sid)
	if !ok {
		t.Fatal("busy turn missing from the registry")
	}
	if st.State != StateRunning {
		t.Fatalf("state = %q, want running", st.State)
	}
	if st.Viewers < 1 {
		t.Fatalf("viewers = %d, want >= 1 (anchor)", st.Viewers)
	}

	close(stop) // let the turn finish
	<-turn.Done()
	if emitted.Load() == 0 {
		t.Fatal("turn never emitted while watched")
	}
}

// TestListAndStop verifies the operator surface: List reflects attach/detach/grace
// transitions, and Stop ends the turn and unblocks an awaiting viewer.
func TestListAndStop(t *testing.T) {
	const sid = libacp.SessionID("s8")
	reg := New(Config{TurnDeadline: 5 * time.Second, GraceWindow: 5 * time.Second, JournalSize: 16})
	defer reg.Close()

	fn := func(ctx context.Context, emit func(context.Context, libacp.SessionNotification)) Result {
		<-ctx.Done()
		return Result{StopReason: libacp.StopReasonCancelled}
	}
	v1 := newRecViewer("v1")
	turn, _, err := reg.Start(sid, fn, v1)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	list := reg.List()
	if len(list) != 1 {
		t.Fatalf("List len = %d, want 1", len(list))
	}
	st := list[0]
	if st.SessionID != sid || st.State != StateRunning || st.Viewers != 1 {
		t.Fatalf("status = %+v, want running/1 viewer for %q", st, sid)
	}
	if st.StartedAt.IsZero() || st.Deadline.IsZero() || !st.Deadline.After(st.StartedAt) {
		t.Fatalf("status times invalid: started=%v deadline=%v", st.StartedAt, st.Deadline)
	}

	// Detach the last viewer: grace state, zero viewers.
	turn.Detach(v1.ID())
	eventually(t, 2*time.Second, func() bool {
		s, ok := reg.Get(sid)
		return ok && s.State == StateGrace && s.Viewers == 0
	})

	// Reattach: back to running.
	v2 := newRecViewer("v2")
	if _, ok, err := reg.AttachIfRunning(context.Background(), sid, v2); err != nil || !ok {
		t.Fatalf("AttachIfRunning v2: ok=%v err=%v", ok, err)
	}
	if s, ok := reg.Get(sid); !ok || s.State != StateRunning || s.Viewers != 1 {
		t.Fatalf("after reattach status = %+v, want running/1", s)
	}

	// A background awaiter, then Stop.
	done := make(chan struct{})
	go func() { <-turn.Done(); close(done) }()
	if !reg.Stop(sid) {
		t.Fatal("Stop returned false for a live turn")
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Stop did not unblock the awaiting viewer")
	}
	if _, ok := reg.Get(sid); ok {
		t.Fatal("session should be gone after Stop")
	}
	if len(reg.List()) != 0 {
		t.Fatal("List should be empty after Stop")
	}
}

// TestStartAttachesToRunningTurn verifies a second Start on an in-flight session
// attaches (started=false) rather than launching a competing turn.
func TestStartAttachesToRunningTurn(t *testing.T) {
	const sid = libacp.SessionID("s9")
	reg := New(Config{TurnDeadline: 5 * time.Second, GraceWindow: 5 * time.Second, JournalSize: 16})
	defer reg.Close()

	var starts atomic.Int32
	fn := func(ctx context.Context, emit func(context.Context, libacp.SessionNotification)) Result {
		starts.Add(1)
		emit(ctx, note(sid, "hello"))
		<-ctx.Done()
		return Result{StopReason: libacp.StopReasonCancelled}
	}
	v1 := newRecViewer("v1")
	if _, started, err := reg.Start(sid, fn, v1); err != nil || !started {
		t.Fatalf("first Start: started=%v err=%v", started, err)
	}
	eventually(t, 2*time.Second, func() bool { return v1.count() == 1 })

	v2 := newRecViewer("v2")
	turn2, started, err := reg.Start(sid, fn, v2)
	if err != nil {
		t.Fatalf("second Start: %v", err)
	}
	if started {
		t.Fatal("second Start should have attached, not started a new turn")
	}
	// v2 replayed the running turn's journal.
	if got := v2.texts(); len(got) != 1 || got[0] != "hello" {
		t.Fatalf("v2 replay = %v, want [hello]", got)
	}
	if starts.Load() != 1 {
		t.Fatalf("turn fn ran %d times, want 1", starts.Load())
	}
	turn2.Cancel()
}

// TestJournalRingEviction verifies Belt 3: the bounded journal keeps only the most
// recent JournalSize events (oldest-first eviction), and a reattaching viewer
// replays exactly that tail with monotonic sequence numbers.
func TestJournalRingEviction(t *testing.T) {
	const sid = libacp.SessionID("s10")
	reg := New(Config{TurnDeadline: 5 * time.Second, GraceWindow: 5 * time.Second, JournalSize: 2})
	defer reg.Close()

	emittedAll := make(chan struct{})
	fn := func(ctx context.Context, emit func(context.Context, libacp.SessionNotification)) Result {
		for i := 1; i <= 4; i++ {
			emit(ctx, note(sid, fmt.Sprintf("e%d", i)))
		}
		close(emittedAll)
		<-ctx.Done()
		return Result{StopReason: libacp.StopReasonCancelled}
	}
	v1 := newRecViewer("v1")
	turn, _, err := reg.Start(sid, fn, v1)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	<-emittedAll

	// A fresh viewer replays only the retained tail (last 2 of 4), with the original
	// sequence numbers preserved.
	v2 := newRecViewer("v2")
	if _, ok, err := reg.AttachIfRunning(context.Background(), sid, v2); err != nil || !ok {
		t.Fatalf("AttachIfRunning v2: ok=%v err=%v", ok, err)
	}
	if got := v2.texts(); len(got) != 2 || got[0] != "e3" || got[1] != "e4" {
		t.Fatalf("v2 replay = %v, want [e3 e4] (oldest evicted)", got)
	}
	if got := v2.seqs(); !equalUint64(got, []uint64{3, 4}) {
		t.Fatalf("v2 seqs = %v, want [3 4]", got)
	}
	turn.Cancel()
}

// TestClose tears down in-flight turns and refuses new Starts.
func TestClose(t *testing.T) {
	const sid = libacp.SessionID("s11")
	reg := New(Config{TurnDeadline: 5 * time.Second, GraceWindow: 5 * time.Second, JournalSize: 16})

	fn := func(ctx context.Context, emit func(context.Context, libacp.SessionNotification)) Result {
		<-ctx.Done()
		return Result{StopReason: libacp.StopReasonCancelled}
	}
	v1 := newRecViewer("v1")
	turn, _, err := reg.Start(sid, fn, v1)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	if err := reg.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	select {
	case <-turn.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("Close did not tear down the in-flight turn")
	}
	if _, _, err := reg.Start("other", fn, newRecViewer("v")); !errors.Is(err, ErrClosed) {
		t.Fatalf("Start after Close err = %v, want ErrClosed", err)
	}
	if err := reg.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

// TestDuplicateViewerRejected verifies a viewer id already attached to a session is
// rejected (attach's exactly-once invariant).
func TestDuplicateViewerRejected(t *testing.T) {
	const sid = libacp.SessionID("s12")
	reg := New(Config{TurnDeadline: 5 * time.Second, GraceWindow: 5 * time.Second, JournalSize: 16})
	defer reg.Close()

	fn := func(ctx context.Context, emit func(context.Context, libacp.SessionNotification)) Result {
		<-ctx.Done()
		return Result{StopReason: libacp.StopReasonCancelled}
	}
	v1 := newRecViewer("dup")
	turn, _, err := reg.Start(sid, fn, v1)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	// Same id attaching again is rejected.
	if _, ok, err := reg.AttachIfRunning(context.Background(), sid, newRecViewer("dup")); err == nil || ok {
		t.Fatalf("duplicate attach: ok=%v err=%v, want rejection", ok, err)
	}
	turn.Cancel()
}

func TestParseEnv(t *testing.T) {
	t.Run("defaults", func(t *testing.T) {
		cfg, err := ParseEnv("", "")
		if err != nil {
			t.Fatalf("ParseEnv: %v", err)
		}
		if cfg.TurnDeadline != DefaultTurnDeadline || cfg.GraceWindow != DefaultGraceWindow {
			t.Fatalf("defaults = %+v", cfg)
		}
	})
	t.Run("valid", func(t *testing.T) {
		cfg, err := ParseEnv("30m", "90s")
		if err != nil {
			t.Fatalf("ParseEnv: %v", err)
		}
		if cfg.TurnDeadline != 30*time.Minute || cfg.GraceWindow != 90*time.Second {
			t.Fatalf("parsed = %+v", cfg)
		}
	})
	t.Run("invalid deadline", func(t *testing.T) {
		if _, err := ParseEnv("nope", ""); err == nil {
			t.Fatal("expected error for invalid CONTENOX_TURN_MAX")
		}
		if _, err := ParseEnv("-5m", ""); err == nil {
			t.Fatal("expected error for negative CONTENOX_TURN_MAX")
		}
	})
	t.Run("invalid grace", func(t *testing.T) {
		if _, err := ParseEnv("", "0s"); err == nil {
			t.Fatal("expected error for non-positive CONTENOX_TURN_GRACE")
		}
	})
}

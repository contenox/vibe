package presence_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/contenox/contenox/libtracker"
	"github.com/contenox/contenox/internal/services/presence"
)

// errStoreBoom is the failure a primed recordingStore returns.
var errStoreBoom = errors.New("boom")

// recordingStore is a ReporterStore test double that counts writes and can be made to fail.
type recordingStore struct {
	mu           sync.Mutex
	registers    []presence.Record
	deregistered bool
	fail         bool
}

func (s *recordingStore) Register(_ context.Context, rec presence.Record) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.registers = append(s.registers, rec)
	if s.fail {
		return errStoreBoom
	}
	return nil
}

func (s *recordingStore) Deregister(_ context.Context, _ presence.Kind, _ string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.deregistered = true
	if s.fail {
		return errStoreBoom
	}
	return nil
}

func (s *recordingStore) writeCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.registers)
}

func (s *recordingStore) lastRegister() (presence.Record, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.registers) == 0 {
		return presence.Record{}, false
	}
	return s.registers[len(s.registers)-1], true
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("condition not met within deadline")
}

func TestUnit_Reporter_WritesOnStartAndFillsIdentity(t *testing.T) {
	store := &recordingStore{}
	r := presence.StartReporter(context.Background(), store, presence.Record{Kind: presence.KindACP})
	t.Cleanup(r.Stop)

	waitFor(t, func() bool { return store.writeCount() >= 1 })

	rec, ok := store.lastRegister()
	if !ok {
		t.Fatal("expected an initial registration")
	}
	if rec.InstanceID == "" {
		t.Error("StartReporter must mint an InstanceID")
	}
	if rec.PID == 0 {
		t.Error("StartReporter must fill PID")
	}
	if rec.StartedAt.IsZero() {
		t.Error("StartReporter must fill StartedAt")
	}
	if rec.LastSeen.IsZero() {
		t.Error("each write must stamp LastSeen")
	}
	if r.InstanceID() != rec.InstanceID {
		t.Errorf("InstanceID accessor %q != registered %q", r.InstanceID(), rec.InstanceID)
	}
}

// TestUnit_Reporter_BestEffort_StoreFailureNeverBreaksStartup pins that a store failing every write never blocks or panics StartReporter.
func TestUnit_Reporter_BestEffort_StoreFailureNeverBreaksStartup(t *testing.T) {
	store := &recordingStore{fail: true}

	done := make(chan *presence.Reporter, 1)
	go func() {
		done <- presence.StartReporter(context.Background(), store,
			presence.Record{Kind: presence.KindServe},
			presence.WithInterval(5*time.Millisecond),
		)
	}()

	var r *presence.Reporter
	select {
	case r = <-done:
	case <-time.After(time.Second):
		t.Fatal("StartReporter blocked on a failing store")
	}
	t.Cleanup(r.Stop)

	// It kept trying despite every write erroring — errors are shrugged, not fatal.
	waitFor(t, func() bool { return store.writeCount() >= 2 })
}

// recordingTracker records the (operation, subject, error) of every report;
// concurrency-safe since the reporter's own goroutine writes while the test reads.
type recordingTracker struct {
	mu     sync.Mutex
	events []trackedEvent
}

type trackedEvent struct {
	op, subject string
	err         error
}

func (r *recordingTracker) Start(_ context.Context, op, subject string, _ ...any) (func(error), func(string, any), func()) {
	return func(err error) {
			r.mu.Lock()
			defer r.mu.Unlock()
			r.events = append(r.events, trackedEvent{op: op, subject: subject, err: err})
		},
		func(string, any) {},
		func() {}
}

func (r *recordingTracker) countFor(op, subject string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, ev := range r.events {
		if ev.op == op && ev.subject == subject && ev.err != nil {
			n++
		}
	}
	return n
}

func (r *recordingTracker) firstFor(op, subject string) (trackedEvent, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, ev := range r.events {
		if ev.op == op && ev.subject == subject && ev.err != nil {
			return ev, true
		}
	}
	return trackedEvent{}, false
}

var _ libtracker.ActivityTracker = (*recordingTracker)(nil)

// TestUnit_Reporter_StoreFailureIsReportedToTracker pins that a failed heartbeat write and a failed deregister are both reported to the tracker.
func TestUnit_Reporter_StoreFailureIsReportedToTracker(t *testing.T) {
	store := &recordingStore{fail: true}
	tracker := &recordingTracker{}
	r := presence.StartReporter(context.Background(), store,
		presence.Record{Kind: presence.KindServe},
		presence.WithInterval(5*time.Millisecond),
		presence.WithTracker(tracker),
	)
	waitFor(t, func() bool { return tracker.countFor("register", "presence_record") >= 1 })

	ev, ok := tracker.firstFor("register", "presence_record")
	if !ok {
		t.Fatal("a failed heartbeat write must be reported")
	}
	if ev.err == nil || !strings.Contains(ev.err.Error(), "heartbeat write failed") {
		t.Errorf("the report must name what failed, got %v", ev.err)
	}
	if !errors.Is(ev.err, errStoreBoom) {
		t.Errorf("the report must carry the store error, got %v", ev.err)
	}

	r.Stop()
	if tracker.countFor("deregister", "presence_record") == 0 {
		t.Error("a failed deregister must be reported too")
	}
}

// TestUnit_Reporter_HealthyStoreReportsNoFailure pins that the tracker hears from the reporter only when a write fails.
func TestUnit_Reporter_HealthyStoreReportsNoFailure(t *testing.T) {
	store := &recordingStore{}
	tracker := &recordingTracker{}
	r := presence.StartReporter(context.Background(), store,
		presence.Record{Kind: presence.KindACP},
		presence.WithInterval(5*time.Millisecond),
		presence.WithTracker(tracker),
	)
	waitFor(t, func() bool { return store.writeCount() >= 2 })
	r.Stop()

	if n := tracker.countFor("register", "presence_record"); n != 0 {
		t.Errorf("a healthy store must report no failures, got %d", n)
	}
	if n := tracker.countFor("deregister", "presence_record"); n != 0 {
		t.Errorf("a healthy deregister must report no failures, got %d", n)
	}
}

func TestUnit_Reporter_UpdateTriggersImmediateHeartbeat(t *testing.T) {
	store := &recordingStore{}
	// A long interval proves the extra write came from Update, not the ticker.
	r := presence.StartReporter(context.Background(), store, presence.Record{Kind: presence.KindACP},
		presence.WithInterval(time.Hour),
	)
	t.Cleanup(r.Stop)
	waitFor(t, func() bool { return store.writeCount() >= 1 })

	r.Update(func(rec *presence.Record) { rec.SessionCount = 3; rec.ClientName = "zed" })

	waitFor(t, func() bool {
		rec, ok := store.lastRegister()
		return ok && rec.SessionCount == 3 && rec.ClientName == "zed"
	})
}

func TestUnit_Reporter_StopDeregisters(t *testing.T) {
	store := &recordingStore{}
	r := presence.StartReporter(context.Background(), store, presence.Record{Kind: presence.KindACP})
	waitFor(t, func() bool { return store.writeCount() >= 1 })

	r.Stop()

	store.mu.Lock()
	dereg := store.deregistered
	store.mu.Unlock()
	if !dereg {
		t.Error("Stop must best-effort deregister the record")
	}
	// Stop is idempotent.
	r.Stop()
}

func TestUnit_Reporter_CtxCancelDeregisters(t *testing.T) {
	store := &recordingStore{}
	ctx, cancel := context.WithCancel(context.Background())
	r := presence.StartReporter(ctx, store, presence.Record{Kind: presence.KindACP})
	waitFor(t, func() bool { return store.writeCount() >= 1 })

	cancel()
	// Stop joins the goroutine; after ctx-cancel the deregister has run.
	r.Stop()

	store.mu.Lock()
	dereg := store.deregistered
	store.mu.Unlock()
	if !dereg {
		t.Error("a cancelled context must best-effort deregister on shutdown")
	}
}

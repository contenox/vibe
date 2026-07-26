package presence_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/contenox/beam/internal/services/presence"
)

// recordingStore is a ReporterStore test double that counts writes and can be
// made to fail, so the best-effort guard is provable without a real db.
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
		return errors.New("boom")
	}
	return nil
}

func (s *recordingStore) Deregister(_ context.Context, _ presence.Kind, _ string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.deregistered = true
	if s.fail {
		return errors.New("boom")
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

// TestUnit_Reporter_BestEffort_StoreFailureNeverBreaksStartup proves the guard:
// a store that fails every write must NOT block StartReporter, must NOT panic,
// and the reporter keeps trying (swallowing errors) — the process it observes is
// entirely unaffected.
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
	r := presence.StartReporter(ctx, store, presence.Record{Kind: presence.KindVSCodeAgent})
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

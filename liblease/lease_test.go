package liblease_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/contenox/contenox/liblease"
)

func leasePath(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "owner.lease")
}

func TestUnit_Lease_AcquireFreeThenHeld(t *testing.T) {
	path := leasePath(t)

	l, err := liblease.Acquire(path, time.Minute)
	if err != nil {
		t.Fatalf("Acquire free: %v", err)
	}
	if l.InstanceID() == "" {
		t.Fatal("expected an instance id")
	}

	// A second acquire while the first is valid must be refused.
	if _, err := liblease.Acquire(path, time.Minute); !errors.Is(err, liblease.ErrHeld) {
		t.Fatalf("second Acquire: want ErrHeld, got %v", err)
	}
}

func TestUnit_Lease_TakeoverAfterExpiry(t *testing.T) {
	path := leasePath(t)

	first, err := liblease.Acquire(path, 40*time.Millisecond)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}

	time.Sleep(70 * time.Millisecond) // let it lapse

	second, err := liblease.Acquire(path, time.Minute)
	if err != nil {
		t.Fatalf("takeover after expiry: %v", err)
	}
	if second.InstanceID() == first.InstanceID() {
		t.Fatal("takeover should mint a new instance id")
	}

	// The original holder has lost it: Renew must report ErrLost.
	if err := first.Renew(); !errors.Is(err, liblease.ErrLost) {
		t.Fatalf("stale Renew: want ErrLost, got %v", err)
	}
}

func TestUnit_Lease_RenewAfterLocalExpiryIsLost(t *testing.T) {
	path := leasePath(t)

	l, err := liblease.Acquire(path, time.Nanosecond)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	time.Sleep(time.Millisecond)

	if err := l.Renew(); !errors.Is(err, liblease.ErrLost) {
		t.Fatalf("expired Renew: want ErrLost, got %v", err)
	}
}

func TestUnit_Lease_RenewKeepsOwnership(t *testing.T) {
	path := leasePath(t)

	l, err := liblease.Acquire(path, 60*time.Millisecond)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	// Renew past the original expiry window; ownership should hold.
	for range 3 {
		time.Sleep(30 * time.Millisecond)
		if err := l.Renew(); err != nil {
			t.Fatalf("Renew: %v", err)
		}
	}
	if l.Expired() {
		t.Fatal("lease should not be expired right after a renew")
	}
	// A challenger still cannot take a renewed, valid lease.
	if _, err := liblease.Acquire(path, time.Minute); !errors.Is(err, liblease.ErrHeld) {
		t.Fatalf("Acquire on renewed lease: want ErrHeld, got %v", err)
	}
}

func TestUnit_Lease_ReleaseFreesIt(t *testing.T) {
	path := leasePath(t)

	l, err := liblease.Acquire(path, time.Minute)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if err := l.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if _, err := liblease.Inspect(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("after Release: want ErrNotExist, got %v", err)
	}
	// Now freely re-acquirable.
	if _, err := liblease.Acquire(path, time.Minute); err != nil {
		t.Fatalf("re-Acquire after Release: %v", err)
	}
}

func TestUnit_Lease_ReleaseAfterTakeoverIsNoop(t *testing.T) {
	path := leasePath(t)

	first, err := liblease.Acquire(path, 30*time.Millisecond)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	time.Sleep(50 * time.Millisecond)
	second, err := liblease.Acquire(path, time.Minute)
	if err != nil {
		t.Fatalf("takeover: %v", err)
	}

	// The original holder releasing must not remove the new holder's lease.
	if err := first.Release(); err != nil {
		t.Fatalf("stale Release: %v", err)
	}
	rec, err := liblease.Inspect(path)
	if err != nil {
		t.Fatalf("Inspect after stale Release: %v", err)
	}
	if rec.InstanceID != second.InstanceID() {
		t.Fatal("stale Release wrongly disturbed the new holder's lease")
	}
}

// TestUnit_Lease_SingleWinnerRace is the core invariant: when many instances
// contend for a free lease at once, exactly one acquires it.
func TestUnit_Lease_SingleWinnerRace(t *testing.T) {
	path := leasePath(t)

	const racers = 25
	var wins int64
	var wg sync.WaitGroup
	bad := make(chan error, racers)

	for range racers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := liblease.Acquire(path, time.Minute)
			switch {
			case err == nil:
				atomic.AddInt64(&wins, 1)
			case errors.Is(err, liblease.ErrHeld):
				// expected for losers
			default:
				bad <- err
			}
		}()
	}
	wg.Wait()
	close(bad)
	for err := range bad {
		t.Fatalf("unexpected acquire error: %v", err)
	}
	if wins != 1 {
		t.Fatalf("expected exactly 1 winner, got %d", wins)
	}
}

// TestUnit_Lease_ExpiredTakeoverSingleWinnerRace covers the stale-record path:
// many challengers may observe the same expired lease, but only one may return
// as owner.
func TestUnit_Lease_ExpiredTakeoverSingleWinnerRace(t *testing.T) {
	const rounds = 20
	const racers = 40

	for round := range rounds {
		path := filepath.Join(t.TempDir(), "expired.lease")
		first, err := liblease.Acquire(path, time.Nanosecond)
		if err != nil {
			t.Fatalf("round %d initial Acquire: %v", round, err)
		}
		time.Sleep(time.Millisecond)

		var wins int64
		var wg sync.WaitGroup
		bad := make(chan error, racers)
		won := make(chan *liblease.Lease, racers)
		start := make(chan struct{})

		for range racers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				l, err := liblease.Acquire(path, time.Minute)
				switch {
				case err == nil:
					atomic.AddInt64(&wins, 1)
					won <- l
				case errors.Is(err, liblease.ErrHeld):
				default:
					bad <- err
				}
			}()
		}
		close(start)
		wg.Wait()
		close(bad)
		close(won)
		for l := range won {
			_ = l.Release()
		}

		for err := range bad {
			t.Fatalf("round %d unexpected acquire error: %v", round, err)
		}
		if wins != 1 {
			t.Fatalf("round %d expected exactly 1 expired-takeover winner, got %d (first=%s)", round, wins, first.InstanceID())
		}
	}
}

// A stalled peer (or a stalled filesystem holding the acquisition lock) must
// not strand the caller: ctx cancellation has to cut the wait short.
func TestUnit_Lease_AcquireContextHonoursDeadline(t *testing.T) {
	path := leasePath(t)
	// Simulate a peer mid-acquisition: the lock directory exists and is fresh,
	// so the staleness reclaim will not fire for another few seconds.
	if err := os.Mkdir(path+".acquire.lock", 0o700); err != nil {
		t.Fatalf("seed acquisition lock: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := liblease.AcquireContext(ctx, path, time.Minute)
	elapsed := time.Since(start)

	if !errors.Is(err, liblease.ErrAcquireTimeout) {
		t.Fatalf("want ErrAcquireTimeout, got %v", err)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want the context cause to be preserved, got %v", err)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("wait was not cancellable: took %s", elapsed)
	}
}

func TestUnit_Lease_AcquireContextRejectsCancelledContext(t *testing.T) {
	path := leasePath(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := liblease.AcquireContext(ctx, path, time.Minute); !errors.Is(err, liblease.ErrAcquireTimeout) {
		t.Fatalf("want ErrAcquireTimeout on a dead context, got %v", err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("a refused acquisition must not have written a lease file")
	}
}

// A cancelled renewal must report "could not check", never ErrLost: a caller
// fencing on ErrLost would otherwise stand down over a shutdown context.
func TestUnit_Lease_RenewContextTimeoutIsNotLost(t *testing.T) {
	path := leasePath(t)
	l, err := liblease.Acquire(path, time.Minute)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if err := os.Mkdir(path+".acquire.lock", 0o700); err != nil {
		t.Fatalf("seed acquisition lock: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	err = l.RenewContext(ctx)
	if !errors.Is(err, liblease.ErrAcquireTimeout) {
		t.Fatalf("want ErrAcquireTimeout, got %v", err)
	}
	if errors.Is(err, liblease.ErrLost) {
		t.Fatal("a timed-out renewal must not be reported as a lost lease")
	}
}

func TestUnit_Lease_ReleaseContextHonoursDeadline(t *testing.T) {
	path := leasePath(t)
	l, err := liblease.Acquire(path, time.Minute)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if err := os.Mkdir(path+".acquire.lock", 0o700); err != nil {
		t.Fatalf("seed acquisition lock: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	if err := l.ReleaseContext(ctx); !errors.Is(err, liblease.ErrAcquireTimeout) {
		t.Fatalf("want ErrAcquireTimeout, got %v", err)
	}
}

// A lock left behind by a dead peer must still be reclaimed once it goes stale,
// so bounding the wait did not turn a recoverable state into a permanent one.
func TestUnit_Lease_AcquireReclaimsStaleAcquisitionLock(t *testing.T) {
	path := leasePath(t)
	if err := os.Mkdir(path+".acquire.lock", 0o700); err != nil {
		t.Fatalf("seed acquisition lock: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// ttl below leaseLockStaleAfter shortens the staleness window to the ttl
	// (floored at 100ms), so the abandoned lock is reclaimed promptly.
	l, err := liblease.AcquireContext(ctx, path, 150*time.Millisecond)
	if err != nil {
		t.Fatalf("Acquire over a stale acquisition lock: %v", err)
	}
	if l.InstanceID() == "" {
		t.Fatal("expected an instance id")
	}
}

// liblease's only wrapper (libprocess.Lock) documents that Renew runs on the
// supervisor's goroutine while Release may come from a caller's Stop, so the
// handle must tolerate that rather than push the mutex onto every consumer.
// Run under -race.
func TestUnit_Lease_HandleIsSafeForConcurrentUse(t *testing.T) {
	path := leasePath(t)
	l, err := liblease.Acquire(path, time.Minute)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}

	var wg sync.WaitGroup
	stop := make(chan struct{})

	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			_ = l.Renew()
		}
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			_ = l.Expired()
			_ = l.Record()
			_ = l.InstanceID()
		}
	}()

	time.Sleep(100 * time.Millisecond)
	if err := l.Release(); err != nil {
		t.Fatalf("concurrent Release: %v", err)
	}
	close(stop)
	wg.Wait()
}

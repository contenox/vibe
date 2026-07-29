package owner

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/contenox/contenox/liblease"
)

func TestUnit_JoinRetriesWhenHeldLeaseIsReleasedBeforeInspect(t *testing.T) {
	origAcquire := acquireLease
	origInspect := inspectLease
	t.Cleanup(func() {
		acquireLease = origAcquire
		inspectLease = origInspect
	})

	leasePath := filepath.Join(t.TempDir(), "modeld.lease")
	acquireCalls := 0
	inspectCalls := 0
	acquireLease = func(path string, ttl time.Duration, opts ...liblease.Option) (*liblease.Lease, error) {
		acquireCalls++
		if acquireCalls == 1 {
			return nil, fmt.Errorf("%w: synthetic holder", liblease.ErrHeld)
		}
		return origAcquire(path, ttl, opts...)
	}
	inspectLease = func(path string) (liblease.Record, error) {
		inspectCalls++
		if inspectCalls == 1 {
			return liblease.Record{}, os.ErrNotExist
		}
		return origInspect(path)
	}

	o, err := Join(context.Background(), Config{
		LeasePath: leasePath,
		TTL:       time.Minute,
		Endpoint:  "127.0.0.1:12345",
	})
	if err != nil {
		t.Fatalf("Join: %v", err)
	}
	t.Cleanup(func() { _ = o.Release() })
	if !o.IsOwner() {
		t.Fatalf("Join returned role %s, want owner", o.Role())
	}
	if acquireCalls != 2 {
		t.Fatalf("acquire calls = %d, want retry after missing inspected lease", acquireCalls)
	}
	if inspectCalls != 1 {
		t.Fatalf("inspect calls = %d, want one missing-lease inspection", inspectCalls)
	}
}

func TestUnit_RenewLoopKeepsLeaseAliveAcrossIntervals(t *testing.T) {
	leasePath := filepath.Join(t.TempDir(), "modeld.lease")
	ttl := 300 * time.Millisecond // interval = ttl/3 = 100ms
	o, err := Join(context.Background(), Config{LeasePath: leasePath, TTL: ttl})
	if err != nil {
		t.Fatalf("Join: %v", err)
	}
	t.Cleanup(func() { _ = o.Release() })

	rec0, err := liblease.Inspect(leasePath)
	if err != nil {
		t.Fatalf("inspect: %v", err)
	}

	time.Sleep(ttl + 50*time.Millisecond) // span ~3 renew intervals

	select {
	case <-o.Lost():
		t.Fatalf("lease lost during healthy renewal: %v", o.LostErr())
	default:
	}
	rec1, err := liblease.Inspect(leasePath)
	if err != nil {
		t.Fatalf("inspect: %v", err)
	}
	if !rec1.RenewedAt.After(rec0.RenewedAt) {
		t.Fatalf("RenewedAt did not advance (%s -> %s): renew loop is not renewing", rec0.RenewedAt, rec1.RenewedAt)
	}
}

func TestUnit_RenewGaveUp_SelfFencesBeforeExpiry(t *testing.T) {
	base := time.Unix(1_000_000, 0)
	validUntil := base.Add(10 * time.Second)
	margin := 2 * time.Second

	// Plenty of validity left: keep retrying, do not forfeit.
	if renewGaveUp(base, validUntil, margin) {
		t.Fatal("renewGaveUp at t0 with 10s left: want keep-trying, got give-up")
	}
	// Just inside the margin (now+margin still before validUntil): keep trying.
	if renewGaveUp(base.Add(7*time.Second), validUntil, margin) {
		t.Fatal("renewGaveUp 3s before expiry with 2s margin: want keep-trying, got give-up")
	}
	// At the margin boundary (now+margin == validUntil): forfeit — never operate
	// past the point a challenger could take over.
	if !renewGaveUp(base.Add(8*time.Second), validUntil, margin) {
		t.Fatal("renewGaveUp exactly at the margin boundary: want give-up, got keep-trying")
	}
	// Past validity entirely: forfeit.
	if !renewGaveUp(base.Add(11*time.Second), validUntil, margin) {
		t.Fatal("renewGaveUp past expiry: want give-up, got keep-trying")
	}
}

func TestUnit_RenewRetryDelay_ClampsAndTightensNearExpiry(t *testing.T) {
	interval := 10 * time.Second
	cases := []struct {
		name      string
		remaining time.Duration
		want      time.Duration
	}{
		{"far out is capped at interval", 40 * time.Second, interval},
		{"half remaining under interval", 4 * time.Second, 2 * time.Second},
		{"near expiry floored", 100 * time.Millisecond, minRenewRetryInterval},
		{"negative remaining floored", -5 * time.Second, minRenewRetryInterval},
	}
	for _, c := range cases {
		if got := renewRetryDelay(interval, c.remaining); got != c.want {
			t.Errorf("%s: renewRetryDelay(%s, %s) = %s, want %s", c.name, interval, c.remaining, got, c.want)
		}
	}
}

func TestUnit_RenewGiveUpMargin_PositiveWithFloor(t *testing.T) {
	if got := renewGiveUpMargin(40 * time.Second); got != 10*time.Second {
		t.Fatalf("renewGiveUpMargin(40s) = %s, want 10s", got)
	}
	// A tiny TTL must still leave a positive, non-trivial margin so the owner
	// self-fences before expiry rather than at it.
	if got := renewGiveUpMargin(400 * time.Millisecond); got != minRenewRetryInterval {
		t.Fatalf("renewGiveUpMargin(400ms) = %s, want floor %s", got, minRenewRetryInterval)
	}
}

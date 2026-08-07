package relaylink

import (
	"testing"
	"time"
)

// TestUnit_BackoffIsExponentialJitteredAndCapped pins the schedule's three
// properties. The jitter is asserted as variance rather than as a
// distribution: the requirement is only that a fleet does not redial in
// lockstep, and identical delays are the sole failure that matters.
func TestUnit_BackoffIsExponentialJitteredAndCapped(t *testing.T) {
	t.Parallel()
	p := Backoff{Initial: 10 * time.Millisecond, Max: 200 * time.Millisecond, Factor: 2, ResetAfter: time.Second}

	distinct := map[time.Duration]bool{}
	for range 64 {
		b := newBackoffState(p)
		var last time.Duration
		for i := range 10 {
			d := b.next()
			ceiling := min(time.Duration(float64(p.Initial)*pow2(i)), p.Max)
			if d <= 0 || d > ceiling {
				t.Fatalf("attempt %d delay %v, outside (0, %v]", i, d, ceiling)
			}
			last = d
		}
		distinct[last] = true
	}
	if len(distinct) < 8 {
		t.Fatalf("only %d distinct capped delays over 64 runs: the jitter is not doing its job", len(distinct))
	}
}

// TestUnit_BackoffResetsAfterAHealthyLink checks the schedule returns to its
// initial ceiling once a connection proved it could stay up, which is what
// keeps an ordinary relay restart from being followed by a slow reconnect.
func TestUnit_BackoffResetsAfterAHealthyLink(t *testing.T) {
	t.Parallel()
	p := Backoff{Initial: 10 * time.Millisecond, Max: 200 * time.Millisecond, Factor: 2, ResetAfter: time.Second}
	b := newBackoffState(p)
	for range 10 {
		b.next()
	}
	b.reset()
	if d := b.next(); d > p.Initial {
		t.Fatalf("delay after reset = %v, want at most %v", d, p.Initial)
	}
}

// TestUnit_BackoffCannotOverflowIntoABusyLoop guards the growth arithmetic: a
// duration that wrapped negative would turn backoff into an unthrottled redial
// loop, which is the one failure mode worse than being slow to reconnect.
func TestUnit_BackoffCannotOverflowIntoABusyLoop(t *testing.T) {
	t.Parallel()
	b := newBackoffState(Backoff{Initial: time.Second, Max: 30 * time.Second, Factor: 1e9, ResetAfter: time.Minute})
	for i := range 1000 {
		if d := b.next(); d <= 0 || d > 30*time.Second {
			t.Fatalf("attempt %d delay %v, outside (0, 30s]", i, d)
		}
	}
}

func pow2(n int) float64 {
	v := 1.0
	for range n {
		v *= 2
	}
	return v
}

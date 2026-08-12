package relaylink

import (
	"testing"
	"time"
)

// TestUnit_Backoff_RetryHintIsClampedAndJittered checks a relay's retry hint
// is clamped to the policy's bounds and drawn with jitter, not applied as a
// fixed delay.
func TestUnit_Backoff_RetryHintIsClampedAndJittered(t *testing.T) {
	policy := Backoff{Initial: time.Second, Max: 30 * time.Second, Factor: 2, ResetAfter: time.Minute}

	for _, tc := range []struct {
		name    string
		hint    time.Duration
		ceiling time.Duration
	}{
		{"within bounds", 10 * time.Second, 10 * time.Second},
		{"above Max is clamped down", 24 * time.Hour, policy.Max},
		{"below Initial is clamped up", time.Millisecond, policy.Initial},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := newBackoffState(policy)
			distinct := make(map[time.Duration]struct{})
			for i := 0; i < 200; i++ {
				b.reset()
				d := b.nextHinted(tc.hint)
				if d <= 0 || d > tc.ceiling {
					t.Fatalf("delay %s outside (0, %s]", d, tc.ceiling)
				}
				distinct[d] = struct{}{}
			}
			if len(distinct) < 2 {
				t.Fatal("delay never varied — a fixed hint returns the whole fleet at once")
			}
		})
	}
}

// TestUnit_Backoff_NoHintFallsThrough checks a non-positive hint falls
// through to the ordinary schedule.
func TestUnit_Backoff_NoHintFallsThrough(t *testing.T) {
	policy := Backoff{Initial: time.Second, Max: 8 * time.Second, Factor: 2, ResetAfter: time.Minute}
	b := newBackoffState(policy)
	for i := 0; i < 10; i++ {
		if d := b.nextHinted(0); d <= 0 || d > policy.Max {
			t.Fatalf("delay %s outside (0, %s]", d, policy.Max)
		}
	}
}

// TestUnit_Backoff_HintDoesNotStallTheSchedule checks the schedule keeps
// advancing under a hint given on every attempt.
func TestUnit_Backoff_HintDoesNotStallTheSchedule(t *testing.T) {
	policy := Backoff{Initial: time.Second, Max: 30 * time.Second, Factor: 2, ResetAfter: time.Minute}
	b := newBackoffState(policy)
	for i := 0; i < 6; i++ {
		b.nextHinted(2 * time.Second)
	}
	if b.ceiling <= policy.Initial {
		t.Fatalf("ceiling still %s after six hinted attempts", b.ceiling)
	}
}

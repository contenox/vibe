package relaylink

import (
	"testing"
	"time"
)

// A relay's retry hint moves the fleet's return without synchronising it, and
// cannot be used to park or to spin a connector: it is clamped to the policy's
// own bounds and drawn with jitter rather than applied as a fixed delay.
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

// No hint means the ordinary schedule, so a relay that says nothing cannot
// accidentally reset a connector that is backing off.
func TestUnit_Backoff_NoHintFallsThrough(t *testing.T) {
	policy := Backoff{Initial: time.Second, Max: 8 * time.Second, Factor: 2, ResetAfter: time.Minute}
	b := newBackoffState(policy)
	for i := 0; i < 10; i++ {
		if d := b.nextHinted(0); d <= 0 || d > policy.Max {
			t.Fatalf("delay %s outside (0, %s]", d, policy.Max)
		}
	}
}

// A relay hinting on every attempt must not hold a failing connector at its
// initial delay: the schedule advances underneath the hint.
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

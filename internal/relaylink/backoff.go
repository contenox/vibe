package relaylink

import (
	"errors"
	"math/rand/v2"
	"time"
)

// Backoff is the redial schedule: exponential, jittered and capped.
//
// The jitter is not decoration. Every runtime paired to one relay redials when
// that relay restarts, and an undithered exponential schedule makes them all
// come back at the same instants — the relay's first moments after a rollout
// are exactly when it can least afford a synchronized fleet. Full jitter
// (uniform over the whole interval, not a small band around it) is what breaks
// the lockstep, at the cost of some attempts being earlier than the nominal
// schedule, which is a cost a connector is happy to pay.
type Backoff struct {
	// Initial is the first delay after a failed attempt.
	Initial time.Duration
	// Max caps the delay. It is a ceiling on how stale a reconnection can
	// be, so it is chosen for "how long after the relay returns is it
	// acceptable to still be down", not for politeness.
	Max time.Duration
	// Factor multiplies the ceiling each consecutive failure.
	Factor float64
	// ResetAfter is how long a link must stay up before the schedule
	// returns to Initial. Without it, a relay that accepts and immediately
	// drops would be redialed at full speed forever, since every attempt
	// technically "succeeded".
	ResetAfter time.Duration
}

// DefaultBackoff is 1s doubling to 30s, resetting after a minute of held
// connection. 1s keeps an ordinary relay restart invisible; 30s bounds
// recovery after a long outage to half a minute while costing an unreachable
// relay two dials a minute per instance, which is nothing.
var DefaultBackoff = Backoff{
	Initial:    time.Second,
	Max:        30 * time.Second,
	Factor:     2,
	ResetAfter: time.Minute,
}

func (b Backoff) withDefaults() Backoff {
	if b.Initial <= 0 {
		b.Initial = DefaultBackoff.Initial
	}
	if b.Max <= 0 {
		b.Max = DefaultBackoff.Max
	}
	if b.Factor <= 1 {
		b.Factor = DefaultBackoff.Factor
	}
	if b.ResetAfter <= 0 {
		b.ResetAfter = DefaultBackoff.ResetAfter
	}
	return b
}

func (b Backoff) validate() error {
	if b.Initial > b.Max {
		return errors.New("relaylink: Backoff Initial exceeds Max")
	}
	return nil
}

// backoffState carries the attempt counter for one supervisor loop. It is not
// safe for concurrent use; exactly one goroutine schedules redials.
type backoffState struct {
	policy  Backoff
	ceiling time.Duration
}

func newBackoffState(p Backoff) *backoffState {
	return &backoffState{policy: p, ceiling: p.Initial}
}

// next returns the delay before the next attempt and advances the ceiling. The
// returned value is uniform in (0, ceiling]; the lower bound is exclusive only
// because a zero delay would make a tight failure loop look like no backoff at
// all in a profile.
func (b *backoffState) next() time.Duration {
	ceiling := b.ceiling
	if ceiling > b.policy.Max {
		ceiling = b.policy.Max
	}
	grown := time.Duration(float64(b.ceiling) * b.policy.Factor)
	if grown > b.policy.Max || grown < b.ceiling {
		// The overflow guard matters: a large Factor and a long-lived
		// connector would otherwise wrap the duration negative and
		// turn backoff into a busy loop.
		grown = b.policy.Max
	}
	b.ceiling = grown
	if ceiling <= 0 {
		return 0
	}
	return time.Duration(rand.Int64N(int64(ceiling))) + 1
}

// reset returns the schedule to its initial delay, after a link proved healthy.
func (b *backoffState) reset() { b.ceiling = b.policy.Initial }

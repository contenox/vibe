package relaylink

import (
	"errors"
	"math/rand/v2"
	"time"
)

// Backoff is the redial schedule: exponential, full-jittered and capped.
type Backoff struct {
	// Initial is the first delay after a failed attempt.
	Initial time.Duration
	// Max caps the delay.
	Max time.Duration
	// Factor multiplies the ceiling each consecutive failure.
	Factor float64
	// ResetAfter is how long a link must stay up before the schedule
	// returns to Initial.
	ResetAfter time.Duration
}

// DefaultBackoff is 1s doubling to 30s, resetting after a minute of held
// connection.
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

type backoffState struct {
	policy  Backoff
	ceiling time.Duration
}

func newBackoffState(p Backoff) *backoffState {
	return &backoffState{policy: p, ceiling: p.Initial}
}

func (b *backoffState) next() time.Duration {
	ceiling := b.ceiling
	if ceiling > b.policy.Max {
		ceiling = b.policy.Max
	}
	grown := time.Duration(float64(b.ceiling) * b.policy.Factor)
	if grown > b.policy.Max || grown < b.ceiling {
		// Guards overflow: a large Factor could wrap the duration negative.
		grown = b.policy.Max
	}
	b.ceiling = grown
	if ceiling <= 0 {
		return 0
	}
	return time.Duration(rand.Int64N(int64(ceiling))) + 1
}

func (b *backoffState) reset() { b.ceiling = b.policy.Initial }

func (b *backoffState) nextHinted(hint time.Duration) time.Duration {
	if hint <= 0 {
		return b.next()
	}
	if hint < b.policy.Initial {
		hint = b.policy.Initial
	}
	if hint > b.policy.Max {
		hint = b.policy.Max
	}
	// The schedule still advances when hinted, so a relay cannot hold the
	// connector at its initial delay.
	b.next()
	return time.Duration(rand.Int64N(int64(hint))) + 1
}

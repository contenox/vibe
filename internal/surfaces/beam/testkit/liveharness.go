// This file is the liveness regression harness: frame-diff instrumentation
// that turns two acceptance metrics — micro-motion above 50% while active,
// a still frame at idle — into `go test` assertions, so a frozen frame or a
// runaway spinner fails CI. render must be a pure function of tick (a fixed
// base time plus tick, never time.Now) so the same tick always yields the
// same output, keeping both assertions deterministic and rerun-stable.
package testkit

import "testing"

// AssertMicroMotion fails t unless more than half of the (ticks-1)
// consecutive tick pairs in render(0)..render(ticks-1) differ from the
// preceding render: an active-turn liveness render must visibly move more
// often than it holds still.
func AssertMicroMotion(t *testing.T, render func(tick int) string, ticks int) {
	t.Helper()
	if ticks < 2 {
		t.Fatalf("testkit: AssertMicroMotion needs ticks >= 2, got %d", ticks)
	}

	prev := render(0)
	differing := 0
	for tick := 1; tick < ticks; tick++ {
		cur := render(tick)
		if cur != prev {
			differing++
		}
		prev = cur
	}

	total := ticks - 1
	if differing*2 <= total {
		t.Fatalf("testkit: micro-motion below 50%%: %d/%d consecutive ticks differed from their predecessor (want > %d)",
			differing, total, total/2)
	}
}

// AssertStabilizes fails t if any consecutive pair in render(0)..
// render(ticks-1) differs: an idle fixture must render the exact same frame
// on every tick, catching both a frozen frame and a spinner that keeps
// advancing after its activity closed.
func AssertStabilizes(t *testing.T, render func(tick int) string, ticks int) {
	t.Helper()
	if ticks < 2 {
		t.Fatalf("testkit: AssertStabilizes needs ticks >= 2, got %d", ticks)
	}

	prev := render(0)
	for tick := 1; tick < ticks; tick++ {
		cur := render(tick)
		if cur != prev {
			t.Fatalf("testkit: frame changed at idle tick %d (want a still frame):\n- %q\n+ %q", tick, prev, cur)
		}
		prev = cur
	}
}

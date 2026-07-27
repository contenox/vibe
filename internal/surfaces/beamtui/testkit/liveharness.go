// This file is the liveness regression harness (blueprint 4.21/4.7): the
// frame-diff instrumentation that turns the blueprint's two acceptance
// metrics — micro-motion above 50% while active, and a still frame at idle
// — into two `go test` assertions, so a liveness regression (the frozen
// frame, or the spinner that never stops) fails CI instead of waiting for a
// human to notice.
//
// render is expected to be a PURE function of tick: production code derives
// tick's "now" deterministically (a fixed base time plus tick advanced by
// liveness's spinner granularity, or whatever cadence the caller's app loop
// ticks at) and renders a liveness.Tracker snapshot through it — never
// time.Now, never a real clock — so calling render with the same tick twice
// yields byte-identical output. That purity is what makes both assertions
// below deterministic and rerun-stable: they compare renders across ticks,
// never across wall-clock time.
package testkit

import "testing"

// AssertMicroMotion fails t unless MORE THAN HALF of the (ticks-1)
// consecutive tick pairs in render(0)..render(ticks-1) differ from their
// immediately preceding render. This is the blueprint 4.7 micro-motion
// criterion applied to a fixed render function instead of a live terminal:
// an active-turn liveness render must visibly move more often than it holds
// still.
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

// AssertStabilizes fails t if ANY consecutive pair in render(0)..
// render(ticks-1) differs. This is the idle counterpart of AssertMicroMotion
// — an idle fixture (no activity open) must render the exact same frame on
// every tick, catching both the frozen-forever bug (trivially, nothing here
// distinguishes it from a correctly still frame) and its opposite, a spinner
// that keeps advancing after the activity it belonged to has closed.
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

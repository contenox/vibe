// Package sessionvitals holds the surface-neutral facts about one live agent
// session — what is happening right now (Tracker), how full the context
// window is (ContextUsage), what deserves the operator's attention (Alerter),
// and what a session is called (Label, Roster). None of it depends on cells,
// ANSI, or a TTY: a terminal and a window must reach the same answer from the
// same event stream, so the answer lives here and each surface only renders
// it. Every value is a pure function of stored state and an injected "now",
// so the whole package tests without a clock, a goroutine, or a channel.
package sessionvitals

import (
	"fmt"
	"time"
)

// Kind names the category of activity a Tracker follows.
type Kind string

const (
	KindTurn     Kind = "turn"
	KindToolCall Kind = "tool_call"
	KindMission  Kind = "mission"
	KindShell    Kind = "shell"
)

// defaultStallAfter is the "no update in this long" threshold NewTracker
// uses when given zero.
const defaultStallAfter = 8 * time.Second

// spinnerTick is the motion granularity Snapshot's SpinnerIndex advances
// on. It is a constant, not a Tracker field, since it defines what "one
// distinct spinner frame" means and must not vary between callers
// comparing frames.
const spinnerTick = 130 * time.Millisecond

// activity is one tracked unit of ongoing work, keyed by its caller-chosen
// id. Once closed it is frozen: lastEventAt and elapsed stop moving, and
// every later Snapshot for it returns the same value regardless of now.
type activity struct {
	kind        Kind
	label       string
	startedAt   time.Time
	lastEventAt time.Time
	open        bool

	// closedElapsed is the elapsed duration at the moment Close froze it;
	// only meaningful when !open.
	closedElapsed time.Duration
}

// Tracker is the single source of truth for "is something happening right
// now". It holds zero goroutines and no clock of its own — every method takes
// the caller's current time explicitly, so the whole liveness story tests as
// pure data transformations. Not safe for concurrent use; a surface drives it
// from one single-threaded event loop.
type Tracker struct {
	stallAfter time.Duration
	activities map[string]*activity
}

// NewTracker returns a Tracker whose stall threshold is stallAfter, or the
// default 8s when stallAfter is zero or negative.
func NewTracker(stallAfter time.Duration) *Tracker {
	if stallAfter <= 0 {
		stallAfter = defaultStallAfter
	}
	return &Tracker{stallAfter: stallAfter, activities: make(map[string]*activity)}
}

// Open starts tracking one activity under id, replacing any previous
// activity registered under the same id (a reused id starts fresh, it
// does not extend the old one's elapsed time).
func (t *Tracker) Open(kind Kind, id, label string, now time.Time) {
	t.activities[id] = &activity{
		kind:        kind,
		label:       label,
		startedAt:   now,
		lastEventAt: now,
		open:        true,
	}
}

// Bump records that id had an event at now (any progress worth resetting
// the stall clock for — a token, a tool-call update, a mission report).
// It is a no-op on an id that was never Open'd or has since Close'd, since
// a stray late event must never resurrect a frozen activity or panic.
func (t *Tracker) Bump(id string, now time.Time) {
	a, ok := t.activities[id]
	if !ok || !a.open {
		return
	}
	a.lastEventAt = now
}

// Close ends activity id, freezing its final elapsed time immediately:
// every later Snapshot for it returns that same frozen elapsed and
// SpinnerIndex, and Stalled forever false. An unknown or already-closed id
// is a no-op for the same race-with-teardown reason Bump is.
func (t *Tracker) Close(id string, now time.Time) {
	a, ok := t.activities[id]
	if !ok || !a.open {
		return
	}
	a.open = false
	a.closedElapsed = now.Sub(a.startedAt)
}

// OpenCount returns the number of activities currently open.
func (t *Tracker) OpenCount() int {
	n := 0
	for _, a := range t.activities {
		if a.open {
			n++
		}
	}
	return n
}

// Ticking reports whether the app loop's tick should be running at all:
// exactly OpenCount() > 0. Callers arm their ticker only while this is true
// and disarm it the instant it goes false, keeping beam flat at idle.
func (t *Tracker) Ticking() bool { return t.OpenCount() > 0 }

// Snapshot is the render-time state one activity-bearing view pulls for
// one activity: everything a component needs to draw its pulse, computed
// fresh from stored timestamps and the caller's now — never accumulated
// or cached.
type Snapshot struct {
	// SpinnerIndex is which frame of a spinnerFrames-long cycle to draw,
	// deterministic in elapsed time so a snapshot sequence is
	// golden-testable motion, not an impression.
	SpinnerIndex int
	// Elapsed is real wall-clock time since the activity opened,
	// formatted "7s" under a minute and "1m07s" at or beyond one.
	Elapsed string
	// Stalled is true once no Bump has landed for at least the Tracker's
	// stallAfter threshold, while still open. A closed activity is never
	// stalled.
	Stalled bool
	// Label is the activity's own text, unconditionally.
	Label string
	// Text is what a view should actually print: Label normally, or a
	// stall phrasing carrying the same real elapsed-since-last-event
	// count once Stalled — same spinner, different text.
	Text string
}

// Snapshot returns the render-time state of activity id at now, or false
// if id is not tracked. spinnerFrames is the caller's spinner cycle length;
// Snapshot never needs the glyphs themselves, only the count.
func (t *Tracker) Snapshot(id string, now time.Time, spinnerFrames int) (Snapshot, bool) {
	a, ok := t.activities[id]
	if !ok {
		return Snapshot{}, false
	}
	return snapshotOf(a, t.stallAfter, now, spinnerFrames), true
}

// Aggregate returns the status line's single line: the count of currently
// open activities and the Snapshot of the oldest (by StartedAt, ties broken
// by id for determinism). ok is false when nothing is open.
func (t *Tracker) Aggregate(now time.Time, spinnerFrames int) (Snapshot, int, bool) {
	var oldest *activity
	var oldestID string
	count := 0
	for id, a := range t.activities {
		if !a.open {
			continue
		}
		count++
		if oldest == nil || a.startedAt.Before(oldest.startedAt) ||
			(a.startedAt.Equal(oldest.startedAt) && id < oldestID) {
			oldest, oldestID = a, id
		}
	}
	if oldest == nil {
		return Snapshot{}, 0, false
	}
	return snapshotOf(oldest, t.stallAfter, now, spinnerFrames), count, true
}

// snapshotOf is Snapshot's pure core, factored out so Aggregate shares it
// exactly rather than re-deriving the same rules.
func snapshotOf(a *activity, stallAfter time.Duration, now time.Time, spinnerFrames int) Snapshot {
	if !a.open {
		return Snapshot{
			SpinnerIndex: spinnerIndex(a.closedElapsed, spinnerFrames),
			Elapsed:      formatElapsed(a.closedElapsed),
			Stalled:      false,
			Label:        a.label,
			Text:         a.label,
		}
	}

	elapsed := now.Sub(a.startedAt)
	sinceEvent := now.Sub(a.lastEventAt)
	stalled := sinceEvent >= stallAfter
	text := a.label
	if stalled {
		text = fmt.Sprintf("still working — no update for %ds", int(sinceEvent/time.Second))
	}
	return Snapshot{
		SpinnerIndex: spinnerIndex(elapsed, spinnerFrames),
		Elapsed:      formatElapsed(elapsed),
		Stalled:      stalled,
		Label:        a.label,
		Text:         text,
	}
}

// spinnerIndex is int(elapsed/spinnerTick) % spinnerFrames: deterministic,
// so the same elapsed duration always names the same frame — the property
// that makes spinner motion golden-testable instead of merely eyeballed.
func spinnerIndex(elapsed time.Duration, spinnerFrames int) int {
	if spinnerFrames <= 0 {
		return 0
	}
	if elapsed < 0 {
		elapsed = 0
	}
	return int(elapsed/spinnerTick) % spinnerFrames
}

// formatElapsed renders d as real wall-clock elapsed time: "Ns" below one
// minute, "MmSSs" (zero-padded seconds) at or beyond it. Elapsed is never
// a fabricated percentage — this is the only formatting liveness does.
func formatElapsed(d time.Duration) string {
	total := int(d / time.Second)
	if total < 0 {
		total = 0
	}
	if total < 60 {
		return fmt.Sprintf("%ds", total)
	}
	return fmt.Sprintf("%dm%02ds", total/60, total%60)
}

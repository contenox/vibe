package nativeturn

import (
	"context"

	"github.com/contenox/beam/libacp"
)

// Event is one captured session/update tagged with a per-session MONOTONIC
// sequence number. The tag is what a future SSE transport's Last-Event-ID replay
// keys off: a viewer that reconnects announcing the last Seq it rendered can be
// resumed from strictly after that point. The in-process WebSocket viewer ignores
// Seq (it replays the whole retained journal on attach), but every event carries it
// so the SSE phase needs no journal-format change.
type Event struct {
	// Seq is the 1-based monotonic id of this event within its session's turn. It
	// increases by one per emitted update and never repeats or reorders.
	Seq uint64
	// Update is the session/update notification as it was emitted by the turn,
	// BEFORE any per-viewer normalization (a viewer applies its own tool-call
	// display normalization on delivery, so replay to a fresh viewer rebuilds that
	// state correctly).
	Update libacp.SessionNotification
}

// Viewer is a consumer attached to one session's live (and replayed) turn stream.
// It is defined here and typed only on this package's Event + libacp so an
// implementer (the acpsvc Transport bridge, a future SSE viewer) needs no import
// beyond the interface and no import cycle can form — the same seam
// agentinstance.Viewer provides for the external path.
type Viewer interface {
	// ID uniquely identifies this viewer WITHIN a session; it is the key the turn
	// registers under and the id Detach later names. Two viewers on one session must
	// not share an ID.
	ID() string

	// Deliver receives one turn event — both the REPLAYED journal backlog (on
	// attach) and every subsequent LIVE event, in order. It MUST NOT block on the
	// turn's behalf: it runs on the turn's fan-out path (or the attaching caller's
	// goroutine for replay) while the session lock is held, so a blocking Deliver
	// stalls the turn and every other viewer of the session. Enqueue or perform a
	// bounded write and return. The returned error is advisory (logged by the caller
	// at most); it never disturbs the turn.
	Deliver(ctx context.Context, ev Event) error
}

// journal is a bounded ring of Events for one session's turn — the structured-event
// counterpart of a terminal's byte scrollback ring. It is NOT safe for concurrent
// use on its own; turnSession serializes every access under its lock, which is what
// makes a viewer's replay-then-join exactly-once and correctly ordered.
//
// append is O(1); snapshot is O(count). Dropping is oldest-first once full, so a
// long turn keeps a fixed-size tail rather than growing without bound (Belt 3).
type journal struct {
	buf   []Event
	start int // index of the oldest retained element
	count int // number of retained elements (<= cap)
}

// newJournal returns a journal retaining at most capacity events. A non-positive
// capacity yields a journal that retains nothing (append is a no-op) — a valid,
// documented degenerate configuration a caller only reaches by explicitly asking
// for it (New floors the Registry's size at DefaultJournalSize).
func newJournal(capacity int) *journal {
	if capacity < 0 {
		capacity = 0
	}
	return &journal{buf: make([]Event, capacity)}
}

// append records ev, evicting the oldest event if the ring is full.
func (j *journal) append(ev Event) {
	c := len(j.buf)
	if c == 0 {
		return
	}
	if j.count < c {
		j.buf[(j.start+j.count)%c] = ev
		j.count++
		return
	}
	// Full: overwrite the oldest and advance the window.
	j.buf[j.start] = ev
	j.start = (j.start + 1) % c
}

// snapshot returns every retained event in arrival order (oldest first) as a fresh
// slice, so a caller can replay it after the owning lock is released — or, as
// turnSession.attach does, under the lock, which forces a concurrent live emit to
// wait and therefore land strictly AFTER the replay.
func (j *journal) snapshot() []Event {
	c := len(j.buf)
	out := make([]Event, j.count)
	for i := 0; i < j.count; i++ {
		out[i] = j.buf[(j.start+i)%c]
	}
	return out
}

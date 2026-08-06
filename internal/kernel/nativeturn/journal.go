package nativeturn

import (
	"context"

	"github.com/contenox/contenox/libacp"
)

// Event is one captured session/update tagged with a per-session monotonic
// sequence number, for a future SSE transport's Last-Event-ID replay. The
// in-process WebSocket viewer ignores Seq today (it replays the whole
// retained journal on attach).
type Event struct {
	// Seq is the 1-based monotonic id of this event within its session's
	// turn. Increases by one per emitted update, never repeats or reorders.
	Seq uint64
	// Update is the session/update notification as emitted by the turn,
	// before any per-viewer normalization.
	Update libacp.SessionNotification
}

// Viewer is a consumer attached to one session's live (and replayed) turn
// stream.
type Viewer interface {
	// ID uniquely identifies this viewer within a session. Two viewers on
	// one session must not share an ID.
	ID() string

	// Deliver receives one turn event, in order: the replayed journal
	// backlog on attach, then every live event.
	//
	// It must not block — it runs on the turn's fan-out path under the
	// session lock, so a blocking call stalls the turn and every other
	// viewer. Enqueue and return. The returned error is advisory only.
	Deliver(ctx context.Context, ev Event) error
}

// journal is a bounded ring of Events for one session's turn. Not safe for
// concurrent use on its own; turnSession serializes every access under its
// lock. append is O(1); snapshot is O(count).
type journal struct {
	buf   []Event
	start int // index of the oldest retained element
	count int // number of retained elements (<= cap)
}

// newJournal returns a journal retaining at most capacity events. A
// non-positive capacity yields a journal that retains nothing (append is a
// no-op).
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

// snapshot returns every retained event in arrival order (oldest first) as a
// fresh slice, so a caller can replay it after the owning lock is released.
func (j *journal) snapshot() []Event {
	c := len(j.buf)
	out := make([]Event, j.count)
	for i := 0; i < j.count; i++ {
		out[i] = j.buf[(j.start+i)%c]
	}
	return out
}

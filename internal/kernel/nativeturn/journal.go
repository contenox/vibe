package nativeturn

import (
	"context"

	"github.com/contenox/contenox/libacp"
)

// Event is one captured session/update tagged with a per-session monotonic sequence
// number, for a future SSE transport's Last-Event-ID replay.
type Event struct {
	// Seq is the 1-based monotonic id of this event within its session's turn.
	Seq uint64
	// Update is the session/update notification as emitted by the turn, before any
	// per-viewer normalization.
	Update libacp.SessionNotification
}

// Viewer is a consumer attached to one session's live (and replayed) turn stream.
type Viewer interface {
	// ID uniquely identifies this viewer within a session; two viewers on one session
	// must not share an ID.
	ID() string

	// Deliver receives one turn event, in order (replayed backlog then live); it must
	// not block since it runs under the session lock, and its returned error is
	// advisory only.
	Deliver(ctx context.Context, ev Event) error
}

type journal struct {
	buf   []Event
	start int
	count int
}

func newJournal(capacity int) *journal {
	if capacity < 0 {
		capacity = 0
	}
	return &journal{buf: make([]Event, capacity)}
}

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
	j.buf[j.start] = ev
	j.start = (j.start + 1) % c
}

func (j *journal) snapshot() []Event {
	c := len(j.buf)
	out := make([]Event, j.count)
	for i := 0; i < j.count; i++ {
		out[i] = j.buf[(j.start+i)%c]
	}
	return out
}

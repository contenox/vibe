package agentinstance

import "github.com/contenox/beam/libacp"

// defaultJournalSize bounds how many recent session/update notifications a
// single session's journal retains for replay to a newly-attached viewer. It is
// the structured-event equivalent of a terminal's bounded
// scrollback. A viewer that attaches
// after this many updates have flowed sees the most recent defaultJournalSize
// events replayed, not the whole history — the ring drops oldest-first.
const defaultJournalSize = 512

// journal is a bounded ring of libacp.SessionNotification for one downstream
// session — the structured-event counterpart to a pty's byte scrollback ring. It is
// NOT safe for concurrent use on its own; the viewerHub serializes every access
// under the owning session's lock.
//
// append is O(1); snapshot is O(count). Dropping is oldest-first once full, so a
// long-lived session keeps a fixed-size tail rather than growing without bound.
type journal struct {
	buf   []libacp.SessionNotification
	start int // index of the oldest retained element
	count int // number of retained elements (<= cap)
}

// newJournal returns a journal retaining at most capacity events. A capacity of
// zero yields a journal that retains nothing (append is a no-op) — a valid,
// documented degenerate configuration.
func newJournal(capacity int) *journal {
	if capacity < 0 {
		capacity = 0
	}
	return &journal{buf: make([]libacp.SessionNotification, capacity)}
}

// append records n, evicting the oldest event if the ring is full.
func (j *journal) append(n libacp.SessionNotification) {
	c := len(j.buf)
	if c == 0 {
		return
	}
	if j.count < c {
		j.buf[(j.start+j.count)%c] = n
		j.count++
		return
	}
	// Full: overwrite the oldest and advance the window.
	j.buf[j.start] = n
	j.start = (j.start + 1) % c
}

// snapshot returns every retained event in arrival order (oldest first) as a
// fresh slice, so a caller can replay it after the owning lock is released.
func (j *journal) snapshot() []libacp.SessionNotification {
	c := len(j.buf)
	out := make([]libacp.SessionNotification, j.count)
	for i := 0; i < j.count; i++ {
		out[i] = j.buf[(j.start+i)%c]
	}
	return out
}

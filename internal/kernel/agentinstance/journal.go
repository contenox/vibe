package agentinstance

import "github.com/contenox/beam/libacp"

// defaultJournalSize bounds how many recent session/update notifications a
// single session's journal retains for replay to a newly-attached viewer. A
// viewer attaching after this many updates sees only the most recent
// defaultJournalSize, oldest dropped first.
const defaultJournalSize = 512

// journal is a bounded ring of libacp.SessionNotification for one downstream
// session. Not safe for concurrent use on its own; the viewerHub serializes
// every access under the owning session's lock. append is O(1); snapshot is
// O(count).
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

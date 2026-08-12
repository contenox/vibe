package agentinstance

import "github.com/contenox/contenox/libacp"

const defaultJournalSize = 512

type journal struct {
	buf   []libacp.SessionNotification
	start int
	count int
}

func newJournal(capacity int) *journal {
	if capacity < 0 {
		capacity = 0
	}
	return &journal{buf: make([]libacp.SessionNotification, capacity)}
}

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
	j.buf[j.start] = n
	j.start = (j.start + 1) % c
}

func (j *journal) snapshot() []libacp.SessionNotification {
	c := len(j.buf)
	out := make([]libacp.SessionNotification, j.count)
	for i := 0; i < j.count; i++ {
		out[i] = j.buf[(j.start+i)%c]
	}
	return out
}

package shellsession

import "sync"

// scrollback is a bounded byte ring with monotonically increasing absolute
// offsets. Every byte the PTY ever emits gets an offset [start, end); once the
// retained window exceeds capacity the oldest bytes are evicted and start
// advances. Because offsets never reset while a shell lives, "read since marker"
// is a cheap, race-free slice: a caller holding offset N asks for everything
// after N and learns the new end, with no risk of re-reading or missing bytes.
type scrollback struct {
	mu       sync.Mutex
	buf      []byte
	start    int64 // absolute offset of buf[0]
	end      int64 // absolute offset one past the last retained byte
	capacity int
}

func newScrollback(capacity int) *scrollback {
	if capacity <= 0 {
		capacity = defaultScrollbackBytes
	}
	return &scrollback{capacity: capacity}
}

// append records p at the current end, evicting the oldest bytes when the
// retained window would exceed capacity. Returns the new end offset.
func (s *scrollback) append(p []byte) int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.buf = append(s.buf, p...)
	s.end += int64(len(p))
	if len(s.buf) > s.capacity {
		drop := len(s.buf) - s.capacity
		// Compact in place so the underlying array does not grow without bound.
		s.buf = append(s.buf[:0], s.buf[drop:]...)
		s.start += int64(drop)
	}
	return s.end
}

// since returns the retained bytes at or after offset, together with the offset
// the returned slice actually starts at (>= offset, clamped up to start when the
// requested offset was already evicted) and the current end. A negative offset,
// or one below start, yields the whole retained window.
func (s *scrollback) since(offset int64) (data []byte, from int64, to int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	from = offset
	if from < s.start {
		from = s.start
	}
	if from > s.end {
		from = s.end
	}
	idx := int(from - s.start)
	out := make([]byte, len(s.buf)-idx)
	copy(out, s.buf[idx:])
	return out, from, s.end
}

// tail returns at most the last n bytes of the retained window.
func (s *scrollback) tail(n int) (data []byte, from int64, to int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if n < 0 {
		n = 0
	}
	idx := 0
	if n < len(s.buf) {
		idx = len(s.buf) - n
	}
	out := make([]byte, len(s.buf)-idx)
	copy(out, s.buf[idx:])
	return out, s.start + int64(idx), s.end
}

// snapshot returns the entire retained window and its bounds.
func (s *scrollback) snapshot() (data []byte, from int64, to int64) {
	return s.since(s.startOffset())
}

func (s *scrollback) startOffset() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.start
}

func (s *scrollback) endOffset() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.end
}

package shellsession

import "sync"

type scrollback struct {
	mu       sync.Mutex
	buf      []byte
	start    int64
	end      int64
	capacity int
}

func newScrollback(capacity int) *scrollback {
	if capacity <= 0 {
		capacity = defaultScrollbackBytes
	}
	return &scrollback{capacity: capacity}
}

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

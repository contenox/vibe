package relaylink

import (
	"errors"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/contenox/contenox/librelay"
)

// Heartbeat governs liveness probing on a held connection.
type Heartbeat struct {
	// Interval is the gap between probes, measured from the previous ack.
	Interval time.Duration
	// Timeout is how long one probe may go unanswered before the
	// connection is declared dead.
	Timeout time.Duration
}

// DefaultHeartbeat probes every 15s and gives up after 10s.
var DefaultHeartbeat = Heartbeat{Interval: 15 * time.Second, Timeout: 10 * time.Second}

func (h Heartbeat) withDefaults() Heartbeat {
	if h.Interval <= 0 {
		h.Interval = DefaultHeartbeat.Interval
	}
	if h.Timeout <= 0 {
		h.Timeout = DefaultHeartbeat.Timeout
	}
	return h
}

func (h Heartbeat) validate() error {
	if h.Interval <= 0 || h.Timeout <= 0 {
		return errors.New("relaylink: Heartbeat Interval and Timeout must be positive")
	}
	return nil
}

type session struct {
	conn net.Conn
	rd   *librelay.Reader
	w    *librelay.Writer

	out  chan librelay.Frame
	done chan struct{}

	closeOnce sync.Once
	first     atomic.Pointer[error]

	seq atomic.Uint64

	note func(id string, data any)

	mu        sync.Mutex
	pendingID string
	pendingCh chan struct{}
}

func (s *session) note0(id string, data any) {
	if s.note != nil {
		s.note(id, data)
	}
}

func newSession(conn net.Conn, rd *librelay.Reader, w *librelay.Writer, backlog int) *session {
	return &session{
		conn: conn,
		rd:   rd,
		w:    w,
		out:  make(chan librelay.Frame, backlog),
		done: make(chan struct{}),
	}
}

func (s *session) stop(reason error) {
	s.closeOnce.Do(func() {
		if reason == nil {
			reason = errors.New("relaylink: connection ended")
		}
		s.first.Store(&reason)
		close(s.done)
		_ = s.conn.Close()
	})
}

func (s *session) reason() error {
	if p := s.first.Load(); p != nil {
		return *p
	}
	return nil
}

func (s *session) enqueue(f librelay.Frame) error {
	select {
	case <-s.done:
		return ErrNotConnected
	default:
	}
	select {
	case s.out <- f:
		return nil
	case <-s.done:
		return ErrNotConnected
	default:
		return ErrBacklogFull
	}
}

func (s *session) writeLoop() {
	for {
		select {
		case f := <-s.out:
			if err := s.w.WriteFrame(f); err != nil {
				s.stop(fmt.Errorf("relaylink: write %q: %w", f.Type, err))
				return
			}
		case <-s.done:
			return
		}
	}
}

func (s *session) arm() (string, <-chan struct{}) {
	id := fmt.Sprintf("hb-%d", s.seq.Add(1))
	ch := make(chan struct{})
	s.mu.Lock()
	s.pendingID, s.pendingCh = id, ch
	s.mu.Unlock()
	return id, ch
}

func (s *session) ack(replyTo string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if replyTo == "" || replyTo != s.pendingID || s.pendingCh == nil {
		return
	}
	close(s.pendingCh)
	s.pendingID, s.pendingCh = "", nil
}

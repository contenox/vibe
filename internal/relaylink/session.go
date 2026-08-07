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
	// Interval is the gap between probes. It is measured from the previous
	// ack, not from the previous send, so a slow peer stretches the period
	// instead of accumulating unanswered probes.
	Interval time.Duration
	// Timeout is how long one probe may go unanswered before the
	// connection is declared dead. It must be shorter than Interval would
	// have to be for the failure to be noticed twice over.
	Timeout time.Duration
}

// DefaultHeartbeat probes every 15s and gives up after 10s. The interval is
// short enough to notice a dead peer before a user does and long enough that a
// large fleet costs a relay nothing; the timeout is well above any plausible
// round trip while still bounding detection to well under a minute.
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

// session is one held connection: the reader, the writer and the state that
// only makes sense while the connection is up. It ends exactly once, and the
// error that ended it is the first one recorded — later failures are
// consequences of the first and would mislead an operator.
type session struct {
	conn net.Conn
	rd   *librelay.Reader
	w    *librelay.Writer

	// out is the outbound queue. It is bounded so a caller can be told
	// "no" instead of being made to wait on a relay.
	out  chan librelay.Frame
	done chan struct{}

	closeOnce sync.Once
	first     atomic.Pointer[error]

	seq atomic.Uint64

	mu        sync.Mutex
	pendingID string
	pendingCh chan struct{}
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

// stop ends the session with reason, unblocking every goroutine on it by
// closing the connection. It is idempotent; the first reason wins.
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

// reason returns why the session ended, or nil while it is live.
func (s *session) reason() error {
	if p := s.first.Load(); p != nil {
		return *p
	}
	return nil
}

// enqueue queues f for the writer goroutine. It never blocks: a closed session
// answers [ErrNotConnected] and a full queue answers [ErrBacklogFull], because
// the alternative is a caller parked on a relay's TCP window.
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

// writeLoop drains the queue onto the connection in order. Blocking here is
// correct and blocking in [session.enqueue] is not: this goroutine has nobody
// waiting on it, and a Close unblocks it by closing the connection.
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

// arm registers the next heartbeat probe and returns its frame ID and a channel
// closed when the matching ack arrives. Correlation is by ID because an ack
// that does not name its probe cannot distinguish a live peer from one that is
// a round behind.
func (s *session) arm() (string, <-chan struct{}) {
	id := fmt.Sprintf("hb-%d", s.seq.Add(1))
	ch := make(chan struct{})
	s.mu.Lock()
	s.pendingID, s.pendingCh = id, ch
	s.mu.Unlock()
	return id, ch
}

// ack releases the probe named by replyTo. An ack for anything else — a stale
// probe from before a timeout, or an ID this end never sent — is ignored.
func (s *session) ack(replyTo string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if replyTo == "" || replyTo != s.pendingID || s.pendingCh == nil {
		return
	}
	close(s.pendingCh)
	s.pendingID, s.pendingCh = "", nil
}

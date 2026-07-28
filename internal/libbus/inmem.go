package libbus

import (
	"context"
	"fmt"
	"log"
	"sync"
	"sync/atomic"
)

// inmemStreamBuffer mirrors the 1024-slot NATS ChanSubscribe buffer, so this
// test/single-process stand-in reproduces NATS's drop-under-backpressure
// policy rather than hiding the bugs that policy is meant to surface.
const inmemStreamBuffer = 1024

// InMem is an in-memory Messenger for single-process use: no NATS, no
// network. It intentionally reproduces the NATS backend's observable
// contract (at-most-once delivery, Request failing immediately with no
// handler registered) — see the Messenger interface docs for the full matrix.
type InMem struct {
	mu       sync.RWMutex
	closed   bool
	streams  map[string][]*inmemSubscription
	handlers map[string]Handler
}

// inmemSubscription owns a per-subscriber queue and the goroutine draining it
// into the caller's channel, decoupling Publish from a slow consumer — without
// it, Publish would block in-line and a stuck consumer could deadlock itself.
type inmemSubscription struct {
	subject string
	ch      chan<- []byte
	inmem   *InMem
	queue   chan []byte
	done    chan struct{}
	// exited is closed by deliver when it returns, so Unsubscribe can promise
	// that no goroutine will touch the caller's channel once it has returned.
	exited  chan struct{}
	once    sync.Once
	dropped atomic.Uint64
}

// NewInMem returns a new in-memory Messenger. Use for local single-process mode (no NATS).
func NewInMem() *InMem {
	return &InMem{
		streams:  make(map[string][]*inmemSubscription),
		handlers: make(map[string]Handler),
	}
}

// Publish hands the message to every Stream subscriber's queue and returns.
// It never blocks on a consumer: a full queue drops the message (see
// inmemStreamBuffer) so that a wedged subscriber cannot take the publisher with it.
func (p *InMem) Publish(ctx context.Context, subject string, data []byte) error {
	// Checked up-front so a cancelled context fails even when nobody is
	// subscribed — the NATS backend behaves the same way.
	if err := ctx.Err(); err != nil {
		return err
	}

	p.mu.RLock()
	if p.closed {
		p.mu.RUnlock()
		return ErrConnectionClosed
	}
	// Copy subscriber list so we don't hold the lock while dispatching.
	subs := make([]*inmemSubscription, len(p.streams[subject]))
	copy(subs, p.streams[subject])
	p.mu.RUnlock()

	for _, sub := range subs {
		sub.enqueue(data)
	}
	return nil
}

// enqueue is the non-blocking hand-off. Drops are logged once per subscription so
// data loss is visible; silently discarding would make backpressure undebuggable.
func (s *inmemSubscription) enqueue(data []byte) {
	select {
	case s.queue <- data:
	case <-s.done:
	default:
		if s.dropped.Add(1) == 1 {
			log.Printf("libbus: in-memory subscriber on subject %q is slow; dropping messages (buffer of %d full)",
				s.subject, cap(s.queue))
		}
	}
}

// Stream creates a subscription to a subject; messages are delivered to ch.
func (p *InMem) Stream(ctx context.Context, subject string, ch chan<- []byte) (Subscription, error) {
	// An already-cancelled context must not yield a live subscription; the NATS
	// backend rejects this case too, so callers can rely on it uniformly.
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	sub := &inmemSubscription{
		subject: subject,
		ch:      ch,
		inmem:   p,
		queue:   make(chan []byte, inmemStreamBuffer),
		done:    make(chan struct{}),
		exited:  make(chan struct{}),
	}

	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil, ErrConnectionClosed
	}
	p.streams[subject] = append(p.streams[subject], sub)
	p.mu.Unlock()

	go sub.deliver()

	go func() {
		select {
		case <-ctx.Done():
			_ = sub.Unsubscribe()
		case <-sub.done:
			// Already unsubscribed; stop watching so this goroutine cannot
			// outlive the subscription when ctx is long-lived.
		}
	}()

	return sub, nil
}

// deliver drains the queue into the caller's channel until the subscription ends.
// Closing exited on the way out is what makes Unsubscribe's no-more-sends
// guarantee real (see Unsubscribe).
func (s *inmemSubscription) deliver() {
	defer close(s.exited)
	for {
		select {
		case <-s.done:
			return
		case data := <-s.queue:
			select {
			case s.ch <- data:
			case <-s.done:
				return
			}
		}
	}
}

// Request invokes the Serve handler registered for the subject, in the caller's
// goroutine. Like the NATS backend it does NOT wait for a handler to appear: a
// missing handler fails immediately rather than after the context deadline.
func (p *InMem) Request(ctx context.Context, subject string, data []byte) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	p.mu.RLock()
	if p.closed {
		p.mu.RUnlock()
		return nil, ErrConnectionClosed
	}
	handler := p.handlers[subject]
	p.mu.RUnlock()

	if handler == nil {
		return nil, ErrRequestTimeout
	}

	reply, err := handler(ctx, data)
	// A cancelled caller must not see success regardless of what the handler
	// returned; other backends surface this from the transport, but the
	// handler runs in-process here, so the check must be explicit.
	if cerr := ctx.Err(); cerr != nil {
		return nil, cerr
	}
	if err != nil {
		// A handler error is a reply, not a transport failure, matching NATS/SQLite.
		return fmt.Appendf(nil, "error: %s", err.Error()), nil
	}
	return reply, nil
}

// Serve registers a handler for the subject. Request calls will invoke this handler.
func (p *InMem) Serve(ctx context.Context, subject string, handler Handler) (Subscription, error) {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil, ErrConnectionClosed
	}
	p.handlers[subject] = handler
	p.mu.Unlock()

	// Subscription that unregisters the handler on Unsubscribe
	sub := &inmemServeSubscription{subject: subject, inmem: p}
	go func() {
		<-ctx.Done()
		_ = sub.Unsubscribe()
	}()

	return sub, nil
}

// Close marks the messenger closed and releases resources.
func (p *InMem) Close() error {
	p.mu.Lock()
	p.closed = true
	subs := make([]*inmemSubscription, 0, len(p.streams))
	for _, list := range p.streams {
		subs = append(subs, list...)
	}
	p.streams = make(map[string][]*inmemSubscription)
	p.handlers = make(map[string]Handler)
	p.mu.Unlock()

	// Stop delivery goroutines outside the lock; stop() re-takes it via Unsubscribe.
	for _, sub := range subs {
		sub.stop()
	}
	return nil
}

func (s *inmemSubscription) Unsubscribe() error {
	s.inmem.mu.Lock()
	subs := s.inmem.streams[s.subject]
	for i, c := range subs {
		if c == s {
			s.inmem.streams[s.subject] = append(subs[:i], subs[i+1:]...)
			break
		}
	}
	s.inmem.mu.Unlock()
	s.stop()
	// Wait for deliver to actually exit, not just be signalled: callers
	// routinely close(ch) right after Unsubscribe, and a still-sending deliver
	// would panic on a closed channel. The SQLite backend's Stream waits too.
	<-s.exited
	return nil
}

// stop halts the delivery goroutine. Safe to call repeatedly and concurrently.
func (s *inmemSubscription) stop() {
	s.once.Do(func() { close(s.done) })
}

type inmemServeSubscription struct {
	subject string
	inmem   *InMem
}

func (s *inmemServeSubscription) Unsubscribe() error {
	s.inmem.mu.Lock()
	delete(s.inmem.handlers, s.subject)
	s.inmem.mu.Unlock()
	return nil
}

var _ Messenger = (*InMem)(nil)

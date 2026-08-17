package libbus

import (
	"context"
	"fmt"
	"log"
	"sync"
	"sync/atomic"
)

// Mirrors the 1024-slot NATS ChanSubscribe buffer, reproducing its
// drop-under-backpressure policy.
const inmemStreamBuffer = 1024

// InMem is an in-memory Messenger for single-process use. It reproduces the NATS
// backend's observable contract.
type InMem struct {
	mu       sync.RWMutex
	closed   bool
	streams  map[string][]*inmemSubscription
	handlers map[string]Handler
}

// inmemSubscription owns a per-subscriber queue and the goroutine draining it
// into the caller's channel, decoupling Publish from a slow consumer.
type inmemSubscription struct {
	subject string
	ch      chan<- []byte
	inmem   *InMem
	queue   chan []byte
	done    chan struct{}
	// exited is closed by deliver when it returns.
	exited  chan struct{}
	once    sync.Once
	dropped atomic.Uint64
}

// NewInMem returns a new in-memory Messenger.
func NewInMem() *InMem {
	return &InMem{
		streams:  make(map[string][]*inmemSubscription),
		handlers: make(map[string]Handler),
	}
}

// Publish hands the message to every Stream subscriber's queue and returns. It
// never blocks on a consumer; a full queue drops the message.
func (p *InMem) Publish(ctx context.Context, subject string, data []byte) error {
	// Checked up-front so a cancelled context fails even when nobody is subscribed.
	if err := ctx.Err(); err != nil {
		return err
	}

	p.mu.RLock()
	if p.closed {
		p.mu.RUnlock()
		return ErrConnectionClosed
	}
	subs := make([]*inmemSubscription, len(p.streams[subject]))
	copy(subs, p.streams[subject])
	p.mu.RUnlock()

	for _, sub := range subs {
		sub.enqueue(data)
	}
	return nil
}

// enqueue is the non-blocking hand-off; drops are logged once per subscription.
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
		}
	}()

	return sub, nil
}

// deliver drains the queue into the caller's channel until the subscription ends.
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
// goroutine. A missing handler fails immediately rather than at the deadline.
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
		return nil, ErrNoResponders
	}

	reply, err := handler(ctx, data)
	// The handler runs in-process here, so the cancellation check is explicit.
	if cerr := ctx.Err(); cerr != nil {
		return nil, cerr
	}
	if err != nil {
		return fmt.Appendf(nil, "error: %s", err.Error()), nil
	}
	return reply, nil
}

// Serve registers a handler for the subject.
func (p *InMem) Serve(ctx context.Context, subject string, handler Handler) (Subscription, error) {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil, ErrConnectionClosed
	}
	p.handlers[subject] = handler
	p.mu.Unlock()

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
	// Wait for deliver to exit: callers routinely close(ch) right after this.
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

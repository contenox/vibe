package libbus

import (
	"context"
	"sync"
)

// InMem is an in-memory implementation of Messenger for single-process use.
// It does not use NATS or any network. Publish delivers to local Stream subscribers;
// Request/Serve work as same-process request-reply.
type InMem struct {
	mu       sync.RWMutex
	closed   bool
	streams  map[string][]chan<- []byte
	handlers map[string]Handler
}

// inmemSubscription removes this subscriber from the stream on Unsubscribe.
type inmemSubscription struct {
	subject string
	ch      chan<- []byte
	inmem   *InMem
}

// NewInMem returns a new in-memory Messenger. Use for local single-process mode (no NATS).
func NewInMem() *InMem {
	return &InMem{
		streams:  make(map[string][]chan<- []byte),
		handlers: make(map[string]Handler),
	}
}

// Publish sends a fire-and-forget message to all Stream subscribers for the subject.
func (p *InMem) Publish(ctx context.Context, subject string, data []byte) error {
	p.mu.RLock()
	if p.closed {
		p.mu.RUnlock()
		return ErrConnectionClosed
	}
	// Copy subscriber list so we don't hold the lock while sending
	subs := make([]chan<- []byte, len(p.streams[subject]))
	copy(subs, p.streams[subject])
	p.mu.RUnlock()

	for _, ch := range subs {
		select {
		case ch <- data:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

// Stream creates a subscription to a subject; messages are delivered to ch.
func (p *InMem) Stream(ctx context.Context, subject string, ch chan<- []byte) (Subscription, error) {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil, ErrConnectionClosed
	}
	if p.streams[subject] == nil {
		p.streams[subject] = make([]chan<- []byte, 0, 1)
	}
	p.streams[subject] = append(p.streams[subject], ch)
	sub := &inmemSubscription{subject: subject, ch: ch, inmem: p}
	p.mu.Unlock()

	go func() {
		<-ctx.Done()
		_ = sub.Unsubscribe()
	}()

	return sub, nil
}

// Request sends a request and waits for a reply from a Serve handler on the subject.
func (p *InMem) Request(ctx context.Context, subject string, data []byte) ([]byte, error) {
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

	// Run handler with context so it can be cancelled
	reply, err := handler(ctx, data)
	if err != nil {
		return nil, err
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
	p.streams = make(map[string][]chan<- []byte)
	p.handlers = make(map[string]Handler)
	p.mu.Unlock()
	return nil
}

func (s *inmemSubscription) Unsubscribe() error {
	s.inmem.mu.Lock()
	defer s.inmem.mu.Unlock()
	subs := s.inmem.streams[s.subject]
	for i, c := range subs {
		if c == s.ch {
			s.inmem.streams[s.subject] = append(subs[:i], subs[i+1:]...)
			break
		}
	}
	return nil
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

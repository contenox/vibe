package libbus

import (
	"context"
	"errors"
)

var (
	// ErrConnectionClosed is returned when an operation is attempted on a closed connection.
	ErrConnectionClosed = errors.New("connection closed")
	// ErrStreamSubscriptionFail is returned when a stream subscription fails.
	ErrStreamSubscriptionFail = errors.New("stream subscription failed")
	// ErrMessagePublish is returned when publishing a message fails for reasons other than a closed connection.
	ErrMessagePublish = errors.New("message publishing failed")
	// ErrRequestTimeout is returned when a request-reply operation times out.
	ErrRequestTimeout = errors.New("request timed out")
)

// Handler processes a request and returns a response for [Messenger.Serve].
type Handler func(ctx context.Context, data []byte) ([]byte, error)

// Messenger is a pub-sub/request-reply interface for distributing lightweight
// messages between services. Every backend guarantees that Publish to a subject
// with no subscribers never blocks, Stream delivers in publish order, a handler
// error still yields a reply, and every method returns ErrConnectionClosed after
// Close. Backends differ in durability, backpressure and latency, so always give
// Request a deadline.
type Messenger interface {
	// Publish sends a fire-and-forget message to a given subject.
	Publish(ctx context.Context, subject string, data []byte) error

	// Stream subscribes to a subject and delivers messages to ch until ctx is
	// cancelled.
	Stream(ctx context.Context, subject string, ch chan<- []byte) (Subscription, error)

	// Request sends a request message and waits for a reply. The context can be
	// used to set a timeout or to cancel the request.
	Request(ctx context.Context, subject string, data []byte) ([]byte, error)

	// Serve registers a handler for a subject; the returned Subscription stops it.
	Serve(ctx context.Context, subject string, handler Handler) (Subscription, error)

	// Close disconnects from the messaging server and releases its resources.
	Close() error
}

// Subscription represents an active subscription to a subject.
type Subscription interface {
	// Unsubscribe removes the subscription, stopping the delivery of messages.
	Unsubscribe() error
}

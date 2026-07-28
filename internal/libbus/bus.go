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

// Handler is a function that processes a request and returns a response.
// It is used by the Serve method to handle incoming requests.
type Handler func(ctx context.Context, data []byte) ([]byte, error)

// Messenger is a high-level pub-sub/request-reply interface for real-time
// notifications and distributing lightweight messages between services.
//
// Guaranteed by every backend (enforced by conformance_test.go): Publish to a
// subject with no subscribers is a no-op and never blocks; Stream delivers in
// publish order until Unsubscribe or context cancel; a handler error still
// yields a reply — Request returns a non-nil error only on transport
// failure, never on handler failure; and after Close, every method returns
// ErrConnectionClosed.
//
// Backends differ in ways callers must tolerate: NATS/InMem are at-most-once
// under backpressure (drop once a subscriber's buffer fills) while SQLiteBus
// is durable; NATS/InMem require Serve to return before Request is called,
// SQLiteBus does not; handler concurrency and delivery latency (SQLiteBus is
// poll-driven) vary per backend. Always give Request a deadline.
type Messenger interface {
	// Publish sends a fire-and-forget message to a given subject.
	Publish(ctx context.Context, subject string, data []byte) error

	// Stream creates a subscription to a subject and delivers messages asynchronously
	// to the provided channel. The subscription is automatically managed and will
	// be closed when the provided context is canceled.
	Stream(ctx context.Context, subject string, ch chan<- []byte) (Subscription, error)

	// Request sends a request message and waits for a reply. The context can be
	// used to set a timeout or to cancel the request.
	Request(ctx context.Context, subject string, data []byte) ([]byte, error)

	// Serve registers a handler for a given subject to respond to requests.
	// It starts a worker that listens for requests and executes the handler.
	// The returned Subscription can be used to stop serving.
	Serve(ctx context.Context, subject string, handler Handler) (Subscription, error)

	// Close disconnects from the messaging server and cleans up any underlying resources.
	Close() error
}

// Subscription represents an active subscription to a subject.
type Subscription interface {
	// Unsubscribe removes the subscription, stopping the delivery of messages.
	Unsubscribe() error
}

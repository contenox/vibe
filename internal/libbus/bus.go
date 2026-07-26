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

// Messenger defines a high-level interface for various messaging patterns.
// It is designed for real-time event notifications, triggering ephemeral tasks,
// and distributing lightweight messages between services.
//
// # Guarantees that hold for every backend
//
// These are asserted by the shared conformance suite (conformance_test.go), so
// callers may rely on them regardless of which implementation is wired in:
//
//   - Publish to a subject with no subscribers succeeds and does nothing.
//   - Publish never blocks on a slow or stuck subscriber, and never blocks
//     long enough for a consumer goroutine to deadlock itself by publishing.
//   - Messages reach a Stream subscriber in publish order.
//   - Unsubscribe (or cancelling the Stream context) stops further delivery.
//   - After Serve returns, a Request on that subject reaches the handler.
//   - A handler that returns an error still produces a reply: Request returns
//     (payload, nil) where the payload is "error: <message>". A non-nil error
//     from Request always means a transport failure, never a handler failure.
//   - Request with a deadline and no handler fails with ErrRequestTimeout;
//     Request whose context is cancelled fails with context.Canceled.
//   - Stream and Publish with an already-cancelled context return an error.
//   - After Close, Publish/Stream/Request return ErrConnectionClosed, and
//     Close is idempotent.
//
// # Documented differences between backends
//
// These are inherent to the transports; they are NOT bugs, and code that must
// run on more than one backend has to be written to tolerate all of them.
//
//   - Delivery under backpressure. NATS and InMem are at-most-once: each
//     subscriber has a 1024-message buffer and messages are DROPPED once it
//     fills (NATS reports this through the ErrorHandler in natsOpts, InMem
//     logs it). SQLiteBus is durable: events are rows, so a slow subscriber
//     falls behind but loses nothing. Do not depend on lossless streaming
//     unless you know you are on SQLite.
//
//   - Late handlers. NATS and InMem resolve the handler at Request time and
//     fail immediately if none is registered — the common startup race
//     "go Serve(...)" followed by Request fails on both. SQLiteBus writes the
//     request to a table, so a handler that registers within the request
//     deadline still picks it up. Always let Serve return before you Request.
//
//   - Handler concurrency. InMem runs the handler synchronously in the
//     caller's goroutine (1 at a time); SQLiteBus processes up to 10 requests
//     per poll tick sequentially in one goroutine; NATS runs handlers
//     concurrently up to maxHandlerConcurrency per subscription. Handlers must
//     therefore be safe for concurrent invocation even though two of the three
//     backends will never actually invoke them concurrently.
//
//   - Latency. NATS and InMem deliver as fast as the transport allows;
//     SQLiteBus is poll-driven (see SQLiteBusOptions), so both events and
//     replies are delayed by up to one poll interval.
//
//   - Request with no handler and no deadline. NATS reports
//     nats.ErrNoResponders, InMem reports ErrRequestTimeout, and SQLiteBus
//     waits for its internal 10s default before reporting ErrRequestTimeout.
//     Always give Request a deadline.
//
//   - Unsubscribe and in-flight messages. SQLiteBus drains events that were
//     already published before returning from Unsubscribe; NATS and InMem
//     discard whatever is still buffered.
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

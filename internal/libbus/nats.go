package libbus

import (
	"context"
	"errors"
	"fmt"
	"log"

	"github.com/nats-io/nats.go"
)

type ps struct {
	nc *nats.Conn
}

type natsSubscription struct {
	sub *nats.Subscription
}

// maxHandlerConcurrency caps how many Serve handlers may run at once per
// subscription. High enough that normal request/reply traffic never queues,
// low enough that a stuck handler cannot exhaust the process.
const maxHandlerConcurrency = 256

type Config struct {
	NATSURL      string
	NATSPassword string
	NATSUser     string
}

func NewPubSub(ctx context.Context, cfg *Config) (Messenger, error) {
	var nc *nats.Conn
	var err error

	natsOpts := []nats.Option{
		nats.ClosedHandler(func(_ *nats.Conn) {
			log.Println("NATS connection closed")
		}),
		nats.DisconnectErrHandler(func(nc *nats.Conn, err error) {
			log.Printf("NATS disconnected. Will autoreconnect: %v", err)
		}),
		nats.ReconnectHandler(func(nc *nats.Conn) {
			log.Printf("NATS reconnected to %s", nc.ConnectedUrl())
		}),
		// Without this, asynchronous errors — above all nats.ErrSlowConsumer, which
		// is how this backend reports that it DISCARDED messages for a subscriber
		// whose buffer overflowed — are swallowed by the client. Data loss would be
		// completely invisible: no log line, no error return, nothing to alert on.
		nats.ErrorHandler(func(_ *nats.Conn, sub *nats.Subscription, err error) {
			subject := "<unknown>"
			if sub != nil {
				subject = sub.Subject
			}
			if errors.Is(err, nats.ErrSlowConsumer) {
				dropped := -1
				if sub != nil {
					if d, derr := sub.Dropped(); derr == nil {
						dropped = d
					}
				}
				log.Printf("NATS slow consumer on subject %s: messages are being dropped (dropped so far: %d)", subject, dropped)
				return
			}
			log.Printf("NATS async error on subject %s: %v", subject, err)
		}),
	}

	if cfg.NATSUser == "" {
		log.Println("Connecting to NATS without authentication")
		nc, err = nats.Connect(cfg.NATSURL, natsOpts...)
	} else {
		log.Printf("Connecting to NATS with user %s", cfg.NATSUser)
		natsOpts = append(natsOpts, nats.UserInfo(cfg.NATSUser, cfg.NATSPassword))
		nc, err = nats.Connect(cfg.NATSURL, natsOpts...)
	}

	if err != nil {
		log.Printf("Failed to connect to NATS at %s: %v", cfg.NATSURL, err)
		return nil, fmt.Errorf("failed to connect to NATS: %w", err)
	}

	log.Printf("Successfully connected to NATS at %s", nc.ConnectedUrl())
	return &ps{nc: nc}, nil
}

func (p *ps) Publish(ctx context.Context, subject string, data []byte) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
		err := p.nc.Publish(subject, data)
		if err != nil {
			if errors.Is(err, nats.ErrConnectionClosed) {
				return ErrConnectionClosed
			}
			return fmt.Errorf("%w: %v", ErrMessagePublish, err)
		}
		return nil
	}
}

func (p *ps) Stream(ctx context.Context, subject string, ch chan<- []byte) (Subscription, error) {
	return p.stream(ctx, subject, "", ch)
}

func (p *ps) stream(ctx context.Context, subject, queue string, ch chan<- []byte) (Subscription, error) {
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}

	if p.nc == nil || p.nc.IsClosed() {
		return nil, ErrConnectionClosed
	}

	natsChan := make(chan *nats.Msg, 1024)
	var sub *nats.Subscription
	var err error

	if queue == "" {
		sub, err = p.nc.ChanSubscribe(subject, natsChan)
	} else {
		sub, err = p.nc.ChanQueueSubscribe(subject, queue, natsChan)
	}

	if err != nil {
		return nil, fmt.Errorf("%w: unable to subscribe to stream %s: %v", ErrStreamSubscriptionFail, subject, err)
	}

	go func() {
		// The NATS client closes natsChan when the subscription is unsubscribed.
		// Closing it here again would cause a panic.
		defer func() {
			if err := sub.Unsubscribe(); err != nil {
				log.Printf("error unsubscribing from stream %s: %v", subject, err)
			}
		}()

		for {
			select {
			case msg, ok := <-natsChan:
				if !ok {
					// Channel was closed by NATS client, exit goroutine.
					return
				}
				select {
				case ch <- msg.Data:
				case <-ctx.Done():
					return
				}
			case <-ctx.Done():
				return
			}
		}
	}()

	return &natsSubscription{sub: sub}, nil
}

func (p *ps) Request(ctx context.Context, subject string, data []byte) ([]byte, error) {
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	msg, err := p.nc.RequestWithContext(ctx, subject, data)
	if err != nil {
		switch {
		case errors.Is(err, context.DeadlineExceeded):
			return nil, ErrRequestTimeout
		case errors.Is(err, nats.ErrConnectionClosed):
			return nil, ErrConnectionClosed
		// Handle the race condition where "no responders" can occur just before a timeout.
		case errors.Is(err, nats.ErrNoResponders):
			// If the context has a deadline, the user's intent was a timeout.
			// Prioritize the timeout error over the "no responders" error in this ambiguous case.
			if _, hasDeadline := ctx.Deadline(); hasDeadline {
				return nil, ErrRequestTimeout
			}
			// Otherwise, if no deadline is set, it's a genuine "no responders" error.
			return nil, err
		default:
			// This correctly propagates other errors, including context.Canceled.
			return nil, err
		}
	}
	return msg.Data, nil
}

func (p *ps) Serve(ctx context.Context, subject string, handler Handler) (Subscription, error) {
	if p.nc == nil || p.nc.IsClosed() {
		return nil, ErrConnectionClosed
	}

	// Bound in-flight handlers. Previously every message spawned an unbounded
	// goroutine, so a burst (or a handler that blocks on a downstream dependency)
	// could grow the goroutine count without limit until the process died — a
	// failure mode that only ever appears in production, since the in-memory and
	// SQLite backends run handlers one at a time. Acquiring the slot inside the
	// NATS callback applies backpressure to the subscription instead: at the cap,
	// dispatch stalls and, if it stays stalled, the client reports a slow consumer
	// through the ErrorHandler registered in natsOpts.
	sem := make(chan struct{}, maxHandlerConcurrency)

	sub, err := p.nc.QueueSubscribe(subject, subject, func(msg *nats.Msg) {
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			return
		}
		go func() {
			defer func() { <-sem }()
			// Add recovery for panics in handler
			defer func() {
				if r := recover(); r != nil {
					log.Printf("handler for subject %s panicked: %v", subject, r)
					errResponse := fmt.Appendf(nil, "error: handler panic: %v", r)
					if pubErr := msg.Respond(errResponse); pubErr != nil {
						log.Printf("failed to publish panic response for subject %s: %v", subject, pubErr)
					}
				}
			}()

			handlerCtx, cancel := context.WithCancel(ctx)
			defer cancel()

			response, err := handler(handlerCtx, msg.Data)
			if err != nil {
				log.Printf("handler for subject %s returned an error: %v", subject, err)
				errResponse := fmt.Appendf(nil, "error: %s", err.Error())
				if pubErr := msg.Respond(errResponse); pubErr != nil {
					log.Printf("failed to publish error response for subject %s: %v", subject, pubErr)
				}
				return
			}

			if pubErr := msg.Respond(response); pubErr != nil {
				log.Printf("failed to publish response for subject %s: %v", subject, pubErr)
			}
		}()
	})

	if err != nil {
		return nil, fmt.Errorf("failed to subscribe for serving requests on %s: %w", subject, err)
	}

	go func() {
		<-ctx.Done()
		if err := sub.Unsubscribe(); err != nil {
			log.Printf("failed to unsubscribe from %s on context done: %v", subject, err)
		}
	}()

	return &natsSubscription{sub: sub}, nil
}

func (p *ps) Close() error {
	if p.nc != nil && !p.nc.IsClosed() {
		p.nc.Close()
	}
	return nil
}

func (s *natsSubscription) Unsubscribe() error {
	return s.sub.Unsubscribe()
}

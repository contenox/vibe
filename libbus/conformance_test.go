package libbus_test

import (
	"context"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	libbus "github.com/contenox/contenox/libbus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// This file is the shared conformance suite for the Messenger interface: every
// backend runs the same matrix, with legitimate differences asserted per backend
// in the divergence section at the bottom. NATS is skipped when Docker is
// unavailable; set LIBBUS_REQUIRE_NATS=1 to turn that skip into a failure.

// newBusFunc builds a fresh Messenger. It registers its own cleanup on t and
// skips the test if the backend's infrastructure is unavailable.
type newBusFunc func(t *testing.T) libbus.Messenger

// backendProps records the behaviours that genuinely differ between backends.
// They are not excuses: each one is asserted by a divergence test below.
type backendProps struct {
	// waitsForLateHandler is true when Request tolerates a handler that is
	// registered after the request was issued (only the durable SQLite backend).
	waitsForLateHandler bool
	// drainsOnUnsubscribe is true when Unsubscribe delivers events published
	// before it was called (only SQLite).
	drainsOnUnsubscribe bool
}

type backend struct {
	name   string
	newBus newBusFunc
	props  backendProps
}

func conformanceBackends() []backend {
	return []backend{
		{
			name: "InMem",
			newBus: func(t *testing.T) libbus.Messenger {
				t.Helper()
				b := libbus.NewInMem()
				t.Cleanup(func() { _ = b.Close() })
				return b
			},
		},
		{
			name: "SQLite",
			newBus: func(t *testing.T) libbus.Messenger {
				t.Helper()
				return newTestBus(t) // fast poll intervals, see sqlite_test.go
			},
			props: backendProps{waitsForLateHandler: true, drainsOnUnsubscribe: true},
		},
		{
			name:   "NATS",
			newBus: newNATSBus,
		},
	}
}

// runConformance executes one behavioural test against every backend.
func runConformance(t *testing.T, name string, fn func(t *testing.T, newBus newBusFunc)) {
	t.Helper()
	for _, be := range conformanceBackends() {
		t.Run(name+"/"+be.name, func(t *testing.T) {
			fn(t, be.newBus)
		})
	}
}

var subjectCounter atomic.Uint64

// uniqueSubject keeps NATS tests isolated: all NATS buses share one server, so
// two tests using the same subject would cross-talk.
func uniqueSubject(t *testing.T) string {
	t.Helper()
	return fmt.Sprintf("conformance.s%d", subjectCounter.Add(1))
}

var (
	natsOnce    sync.Once
	natsURL     string
	natsErr     error
	natsCleanup = func() {}
)

// newNATSBus connects to a NATS server started once for the whole package.
// Starting a container per test would dominate the suite's runtime.
func newNATSBus(t *testing.T) libbus.Messenger {
	t.Helper()
	natsOnce.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer cancel()
		url, _, cleanup, err := libbus.SetupNatsInstance(ctx)
		natsURL, natsErr, natsCleanup = url, err, cleanup
	})
	if natsErr != nil {
		msg := fmt.Sprintf("NATS backend NOT exercised: could not start the nats:2.10 container (%v). "+
			"Docker is required to run the NATS half of the conformance suite.", natsErr)
		if os.Getenv("LIBBUS_REQUIRE_NATS") != "" {
			t.Fatal(msg)
		}
		t.Skip(msg)
	}
	bus, err := libbus.NewPubSub(context.Background(), &libbus.Config{NATSURL: natsURL})
	require.NoError(t, err)
	t.Cleanup(func() { _ = bus.Close() })
	return bus
}

func TestMain(m *testing.M) {
	code := m.Run()
	natsCleanup()
	os.Exit(code)
}

// receive waits for one message, failing the test rather than hanging forever.
func receive(t *testing.T, ch <-chan []byte, within time.Duration) []byte {
	t.Helper()
	select {
	case msg := <-ch:
		return msg
	case <-time.After(within):
		t.Fatal("timed out waiting for a message")
		return nil
	}
}

// expectNoMessage asserts nothing arrives during the window. The window has to
// exceed the SQLite poll interval, otherwise it proves nothing there.
func expectNoMessage(t *testing.T, ch <-chan []byte, within time.Duration) {
	t.Helper()
	select {
	case msg := <-ch:
		t.Fatalf("unexpected message: %q", string(msg))
	case <-time.After(within):
	}
}

const settle = 250 * time.Millisecond

func TestConformance_PublishStreamDelivery(t *testing.T) {
	runConformance(t, "publish_is_delivered_to_subscriber", func(t *testing.T, newBus newBusFunc) {
		ctx := t.Context()
		bus := newBus(t)
		subject := uniqueSubject(t)

		ch := make(chan []byte, 4)
		sub, err := bus.Stream(ctx, subject, ch)
		require.NoError(t, err)
		defer sub.Unsubscribe()

		require.NoError(t, bus.Publish(ctx, subject, []byte("hello")))
		assert.Equal(t, "hello", string(receive(t, ch, 5*time.Second)))
	})
}

func TestConformance_PublishPreservesOrder(t *testing.T) {
	runConformance(t, "messages_arrive_in_publish_order", func(t *testing.T, newBus newBusFunc) {
		ctx := t.Context()
		bus := newBus(t)
		subject := uniqueSubject(t)

		ch := make(chan []byte, 64)
		sub, err := bus.Stream(ctx, subject, ch)
		require.NoError(t, err)
		defer sub.Unsubscribe()

		const n = 20
		for i := range n {
			require.NoError(t, bus.Publish(ctx, subject, fmt.Appendf(nil, "msg-%02d", i)))
		}

		// Fits comfortably inside every backend's subscriber buffer, so nothing
		// may be dropped here even on the at-most-once backends.
		got := make([]string, 0, n)
		for range n {
			got = append(got, string(receive(t, ch, 5*time.Second)))
		}
		want := make([]string, 0, n)
		for i := range n {
			want = append(want, fmt.Sprintf("msg-%02d", i))
		}
		assert.Equal(t, want, got)
	})
}

func TestConformance_MultipleSubscribers(t *testing.T) {
	runConformance(t, "every_subscriber_gets_every_message", func(t *testing.T, newBus newBusFunc) {
		ctx := t.Context()
		bus := newBus(t)
		subject := uniqueSubject(t)

		chA := make(chan []byte, 4)
		chB := make(chan []byte, 4)
		subA, err := bus.Stream(ctx, subject, chA)
		require.NoError(t, err)
		defer subA.Unsubscribe()
		subB, err := bus.Stream(ctx, subject, chB)
		require.NoError(t, err)
		defer subB.Unsubscribe()

		require.NoError(t, bus.Publish(ctx, subject, []byte("fanout")))

		assert.Equal(t, "fanout", string(receive(t, chA, 5*time.Second)))
		assert.Equal(t, "fanout", string(receive(t, chB, 5*time.Second)))
	})
}

func TestConformance_SubjectIsolation(t *testing.T) {
	runConformance(t, "subscriber_does_not_see_other_subjects", func(t *testing.T, newBus newBusFunc) {
		ctx := t.Context()
		bus := newBus(t)
		mine, theirs := uniqueSubject(t), uniqueSubject(t)

		ch := make(chan []byte, 4)
		sub, err := bus.Stream(ctx, mine, ch)
		require.NoError(t, err)
		defer sub.Unsubscribe()

		require.NoError(t, bus.Publish(ctx, theirs, []byte("not for you")))
		expectNoMessage(t, ch, settle)
	})
}

func TestConformance_PublishWithNoSubscribers(t *testing.T) {
	runConformance(t, "publish_without_subscribers_succeeds", func(t *testing.T, newBus newBusFunc) {
		ctx := t.Context()
		bus := newBus(t)
		// Fire-and-forget: nobody listening is not an error on any backend.
		require.NoError(t, bus.Publish(ctx, uniqueSubject(t), []byte("into the void")))
	})
}

func TestConformance_UnsubscribeStopsDelivery(t *testing.T) {
	runConformance(t, "no_delivery_after_unsubscribe", func(t *testing.T, newBus newBusFunc) {
		ctx := t.Context()
		bus := newBus(t)
		subject := uniqueSubject(t)

		ch := make(chan []byte, 8)
		sub, err := bus.Stream(ctx, subject, ch)
		require.NoError(t, err)

		require.NoError(t, bus.Publish(ctx, subject, []byte("before")))
		assert.Equal(t, "before", string(receive(t, ch, 5*time.Second)))

		require.NoError(t, sub.Unsubscribe())
		require.NoError(t, bus.Publish(ctx, subject, []byte("after")))
		expectNoMessage(t, ch, settle)
	})
}

func TestConformance_StreamContextCancelStopsDelivery(t *testing.T) {
	runConformance(t, "cancelling_stream_context_stops_delivery", func(t *testing.T, newBus newBusFunc) {
		bus := newBus(t)
		subject := uniqueSubject(t)

		streamCtx, cancel := context.WithCancel(t.Context())
		ch := make(chan []byte, 8)
		sub, err := bus.Stream(streamCtx, subject, ch)
		require.NoError(t, err)
		defer sub.Unsubscribe()

		require.NoError(t, bus.Publish(t.Context(), subject, []byte("before")))
		assert.Equal(t, "before", string(receive(t, ch, 5*time.Second)))

		cancel()
		time.Sleep(settle) // let the backend tear the subscription down

		require.NoError(t, bus.Publish(t.Context(), subject, []byte("after")))
		expectNoMessage(t, ch, settle)
	})
}

func TestConformance_StreamWithCancelledContext(t *testing.T) {
	runConformance(t, "stream_with_cancelled_context_errors", func(t *testing.T, newBus newBusFunc) {
		bus := newBus(t)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		_, err := bus.Stream(ctx, uniqueSubject(t), make(chan []byte, 1))
		require.ErrorIs(t, err, context.Canceled)
	})
}

func TestConformance_PublishWithCancelledContext(t *testing.T) {
	runConformance(t, "publish_with_cancelled_context_errors", func(t *testing.T, newBus newBusFunc) {
		bus := newBus(t)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		// Must fail even though nobody is subscribed: the failure has to depend
		// on the caller's context, not on whether a subscriber happens to exist.
		require.Error(t, bus.Publish(ctx, uniqueSubject(t), []byte("data")))
	})
}

func TestConformance_PublishDoesNotBlockOnStuckConsumer(t *testing.T) {
	runConformance(t, "publish_never_blocks_on_a_stuck_consumer", func(t *testing.T, newBus newBusFunc) {
		ctx := t.Context()
		bus := newBus(t)
		subject := uniqueSubject(t)

		// Unbuffered channel that is never read: the worst kind of consumer.
		// Whether the messages are dropped (NATS/InMem) or kept (SQLite) is a
		// documented difference; that the publisher survives is not negotiable.
		stuck := make(chan []byte)
		sub, err := bus.Stream(ctx, subject, stuck)
		require.NoError(t, err)
		defer sub.Unsubscribe()

		done := make(chan error, 1)
		go func() {
			for range 20 {
				if err := bus.Publish(ctx, subject, []byte("backpressure")); err != nil {
					done <- err
					return
				}
			}
			done <- nil
		}()

		select {
		case err := <-done:
			require.NoError(t, err)
		case <-time.After(5 * time.Second):
			t.Fatal("Publish blocked on a stuck consumer — a slow subscriber must not stall the publisher")
		}
	})
}

func TestConformance_RequestReply(t *testing.T) {
	runConformance(t, "request_reaches_handler_and_returns_reply", func(t *testing.T, newBus newBusFunc) {
		ctx := t.Context()
		bus := newBus(t)
		subject := uniqueSubject(t)

		sub, err := bus.Serve(ctx, subject, func(_ context.Context, data []byte) ([]byte, error) {
			return append([]byte("echo:"), data...), nil
		})
		require.NoError(t, err)
		defer sub.Unsubscribe()

		// No sleep: once Serve has returned, a Request must reach the handler.
		reqCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		reply, err := bus.Request(reqCtx, subject, []byte("ping"))
		require.NoError(t, err)
		assert.Equal(t, "echo:ping", string(reply))
	})
}

func TestConformance_RequestWithNoHandler(t *testing.T) {
	runConformance(t, "request_without_handler_times_out", func(t *testing.T, newBus newBusFunc) {
		bus := newBus(t)
		ctx, cancel := context.WithTimeout(t.Context(), 300*time.Millisecond)
		defer cancel()

		_, err := bus.Request(ctx, uniqueSubject(t), []byte("anyone there?"))
		require.ErrorIs(t, err, libbus.ErrRequestTimeout)
	})
}

func TestConformance_RequestHandlerErrorBecomesReply(t *testing.T) {
	runConformance(t, "handler_error_is_a_reply_not_a_transport_error", func(t *testing.T, newBus newBusFunc) {
		ctx := t.Context()
		bus := newBus(t)
		subject := uniqueSubject(t)

		sub, err := bus.Serve(ctx, subject, func(_ context.Context, _ []byte) ([]byte, error) {
			return nil, fmt.Errorf("handler exploded")
		})
		require.NoError(t, err)
		defer sub.Unsubscribe()

		reqCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		reply, err := bus.Request(reqCtx, subject, []byte("boom"))
		// A non-nil error from Request must mean transport failure, never a
		// handler failure — otherwise the same caller code branches differently
		// depending on which backend is wired in.
		require.NoError(t, err)
		assert.Equal(t, "error: handler exploded", string(reply))
	})
}

func TestConformance_RequestWithCancelledContext(t *testing.T) {
	runConformance(t, "request_with_cancelled_context_reports_cancellation", func(t *testing.T, newBus newBusFunc) {
		bus := newBus(t)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		_, err := bus.Request(ctx, uniqueSubject(t), []byte("data"))
		require.ErrorIs(t, err, context.Canceled)
	})
}

func TestConformance_RequestInFlightCancellation(t *testing.T) {
	runConformance(t, "cancelling_an_inflight_request_unblocks_the_caller", func(t *testing.T, newBus newBusFunc) {
		bus := newBus(t)
		subject := uniqueSubject(t)

		sub, err := bus.Serve(t.Context(), subject, func(ctx context.Context, _ []byte) ([]byte, error) {
			select {
			case <-time.After(5 * time.Second):
			case <-ctx.Done():
			}
			return []byte("late"), nil
		})
		require.NoError(t, err)
		defer sub.Unsubscribe()

		reqCtx, cancel := context.WithCancel(t.Context())
		errCh := make(chan error, 1)
		go func() {
			_, err := bus.Request(reqCtx, subject, []byte("data"))
			errCh <- err
		}()

		time.Sleep(100 * time.Millisecond)
		cancel()

		select {
		case err := <-errCh:
			require.Error(t, err, "a cancelled request must not report success")
		case <-time.After(10 * time.Second):
			t.Fatal("Request did not return after its context was cancelled")
		}
	})
}

func TestConformance_ServeContextCancelStopsHandling(t *testing.T) {
	runConformance(t, "cancelling_serve_context_stops_handling", func(t *testing.T, newBus newBusFunc) {
		bus := newBus(t)
		subject := uniqueSubject(t)

		serveCtx, cancel := context.WithCancel(t.Context())
		var calls atomic.Int64
		_, err := bus.Serve(serveCtx, subject, func(_ context.Context, _ []byte) ([]byte, error) {
			calls.Add(1)
			return []byte("pong"), nil
		})
		require.NoError(t, err)

		cancel()
		time.Sleep(settle) // let the backend deregister the handler

		reqCtx, reqCancel := context.WithTimeout(t.Context(), 500*time.Millisecond)
		defer reqCancel()
		_, err = bus.Request(reqCtx, subject, []byte("ping"))
		require.Error(t, err, "a request must not be served after the Serve context was cancelled")
		assert.Zero(t, calls.Load(), "handler ran after its context was cancelled")
	})
}

func TestConformance_ServeUnsubscribeStopsHandling(t *testing.T) {
	runConformance(t, "unsubscribing_a_handler_stops_handling", func(t *testing.T, newBus newBusFunc) {
		ctx := t.Context()
		bus := newBus(t)
		subject := uniqueSubject(t)

		sub, err := bus.Serve(ctx, subject, func(_ context.Context, _ []byte) ([]byte, error) {
			return []byte("pong"), nil
		})
		require.NoError(t, err)

		reqCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		reply, err := bus.Request(reqCtx, subject, []byte("ping"))
		cancel()
		require.NoError(t, err)
		require.Equal(t, "pong", string(reply))

		require.NoError(t, sub.Unsubscribe())
		time.Sleep(settle)

		reqCtx2, cancel2 := context.WithTimeout(ctx, 500*time.Millisecond)
		defer cancel2()
		_, err = bus.Request(reqCtx2, subject, []byte("ping"))
		require.Error(t, err, "handler still answered after Unsubscribe")
	})
}

func TestConformance_SequentialRequests(t *testing.T) {
	runConformance(t, "handler_serves_repeated_requests", func(t *testing.T, newBus newBusFunc) {
		ctx := t.Context()
		bus := newBus(t)
		subject := uniqueSubject(t)

		var served atomic.Int64
		sub, err := bus.Serve(ctx, subject, func(_ context.Context, _ []byte) ([]byte, error) {
			// Handlers must be concurrency-safe: NATS may run several at once,
			// even though InMem and SQLite never do.
			return fmt.Appendf(nil, "%d", served.Add(1)), nil
		})
		require.NoError(t, err)
		defer sub.Unsubscribe()

		for i := 1; i <= 5; i++ {
			reqCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
			reply, err := bus.Request(reqCtx, subject, nil)
			cancel()
			require.NoError(t, err)
			assert.Equal(t, fmt.Sprintf("%d", i), string(reply))
		}
	})
}

func TestConformance_CloseSemantics(t *testing.T) {
	runConformance(t, "operations_after_close_report_connection_closed", func(t *testing.T, newBus newBusFunc) {
		ctx := context.Background()
		bus := newBus(t)
		subject := uniqueSubject(t)

		// Live subscriptions whose context outlives the bus: Close must tear them
		// down itself rather than wait on a context that will never be cancelled.
		_, err := bus.Stream(ctx, subject, make(chan []byte, 1)) //nolint:errcheck // subscription is torn down by Close
		require.NoError(t, err)
		_, err = bus.Serve(ctx, subject, func(_ context.Context, _ []byte) ([]byte, error) {
			return nil, nil
		})
		require.NoError(t, err)

		closed := make(chan error, 1)
		go func() { closed <- bus.Close() }()
		select {
		case err := <-closed:
			require.NoError(t, err)
		case <-time.After(10 * time.Second):
			t.Fatal("Close blocked with live subscriptions outstanding")
		}

		require.ErrorIs(t, bus.Publish(ctx, subject, []byte("data")), libbus.ErrConnectionClosed)

		_, err = bus.Stream(ctx, subject, make(chan []byte, 1))
		require.ErrorIs(t, err, libbus.ErrConnectionClosed)

		reqCtx, cancel := context.WithTimeout(ctx, time.Second)
		defer cancel()
		_, err = bus.Request(reqCtx, subject, []byte("data"))
		require.ErrorIs(t, err, libbus.ErrConnectionClosed)
	})
}

func TestConformance_CloseIsIdempotent(t *testing.T) {
	runConformance(t, "close_twice_is_safe", func(t *testing.T, newBus newBusFunc) {
		bus := newBus(t)
		require.NoError(t, bus.Close())
		require.NoError(t, bus.Close())
	})
}

//
// These assert the differences documented on the Messenger interface. They
// exist so that a backend which quietly changes behaviour breaks a test
// instead of breaking a caller in production.

func TestConformanceDivergence_LateHandler(t *testing.T) {
	// The classic startup race: `go Serve(...)` followed immediately by Request.
	// Only the durable SQLite backend survives it — NATS resolves responders at
	// request time and fails within microseconds, and InMem matches NATS on
	// purpose so that code developed against InMem fails the same way it would
	// in production rather than passing locally and breaking on NATS.
	for _, be := range conformanceBackends() {
		t.Run(be.name, func(t *testing.T) {
			bus := be.newBus(t)
			subject := uniqueSubject(t)

			go func() {
				time.Sleep(200 * time.Millisecond)
				_, _ = bus.Serve(context.Background(), subject, func(_ context.Context, _ []byte) ([]byte, error) {
					return []byte("pong"), nil
				})
			}()

			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			reply, err := bus.Request(ctx, subject, []byte("ping"))

			if be.props.waitsForLateHandler {
				require.NoError(t, err, "%s is documented to wait for a late handler", be.name)
				assert.Equal(t, "pong", string(reply))
				return
			}
			require.ErrorIs(t, err, libbus.ErrRequestTimeout,
				"%s is documented to fail fast when no handler is registered yet", be.name)
			require.ErrorIs(t, err, libbus.ErrNoResponders,
				"%s must report a missing handler as ErrNoResponders, which is what lets a caller retry the request without risking a second execution", be.name)
		})
	}
}

func TestConformanceDivergence_UnsubscribeDrain(t *testing.T) {
	// SQLite drains events published before Unsubscribe; NATS and InMem discard
	// whatever is still buffered. Callers that need the last event must not
	// assume the SQLite behaviour.
	for _, be := range conformanceBackends() {
		t.Run(be.name, func(t *testing.T) {
			if !be.props.drainsOnUnsubscribe {
				t.Skipf("%s does not promise to drain buffered events on Unsubscribe", be.name)
			}
			ctx := t.Context()
			bus := be.newBus(t)
			subject := uniqueSubject(t)

			ch := make(chan []byte, 4)
			sub, err := bus.Stream(ctx, subject, ch)
			require.NoError(t, err)

			require.NoError(t, bus.Publish(ctx, subject, []byte("last-event")))
			require.NoError(t, sub.Unsubscribe())

			select {
			case msg := <-ch:
				assert.Equal(t, "last-event", string(msg))
			default:
				t.Fatal("drain-on-unsubscribe lost the pending event")
			}
		})
	}
}

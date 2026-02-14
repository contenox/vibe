package libbus_test

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	libbus "github.com/contenox/vibe/libbus"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/require"
)

func TestSystem_Publish_ContextCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Cancel immediately

	ps, cleanup, err := libbus.NewTestPubSub()
	require.NoError(t, err)
	defer cleanup()

	err = ps.Publish(ctx, "test.canceled", []byte("data"))
	require.ErrorIs(t, err, context.Canceled)
}

func TestSystem_Stream_ContextCanceledBeforeCall(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	ps, cleanup, err := libbus.NewTestPubSub()
	require.NoError(t, err)
	defer cleanup()

	ch := make(chan []byte, 1)
	_, err = ps.Stream(ctx, "test.canceled", ch)
	require.ErrorIs(t, err, context.Canceled)
}

func TestSystem_Request_ContextCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	ps, cleanup, err := libbus.NewTestPubSub()
	require.NoError(t, err)
	defer cleanup()

	subject := "test.request.canceled"

	// Set up a handler that will sleep before responding
	handler := func(ctx context.Context, data []byte) ([]byte, error) {
		// Sleep longer than our cancellation delay
		time.Sleep(500 * time.Millisecond)
		return []byte("response"), nil
	}

	// Start the handler
	sub, err := ps.Serve(ctx, subject, handler)
	require.NoError(t, err)
	defer sub.Unsubscribe()

	// Start request in goroutine
	errCh := make(chan error, 1)
	go func() {
		_, err := ps.Request(ctx, subject, []byte("data"))
		errCh <- err
	}()

	// Cancel after short delay to ensure request is in-flight
	// but before handler can respond
	time.Sleep(100 * time.Millisecond)
	cancel()

	select {
	case err := <-errCh:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(1 * time.Second):
		t.Fatal("request didn't return after cancellation")
	}
}

func TestSystem_Stream_ConnectionClosed(t *testing.T) {
	ps, cleanup, err := libbus.NewTestPubSub()
	require.NoError(t, err)
	require.NoError(t, ps.Close()) // Close connection
	cleanup()

	ch := make(chan []byte, 1)
	_, err = ps.Stream(context.Background(), "test.closed", ch)
	require.ErrorIs(t, err, libbus.ErrConnectionClosed)
}

// 5. Test Serve with closed connection
func TestSystem_Serve_ConnectionClosed(t *testing.T) {
	ps, cleanup, err := libbus.NewTestPubSub()
	require.NoError(t, err)
	require.NoError(t, ps.Close()) // Close connection
	cleanup()

	handler := func(ctx context.Context, data []byte) ([]byte, error) {
		return nil, nil
	}

	_, err = ps.Serve(context.Background(), "test.closed", handler)
	require.ErrorIs(t, err, libbus.ErrConnectionClosed)
}

func TestSystem_Request_NoResponder_NoDeadline(t *testing.T) {
	ps, cleanup, err := libbus.NewTestPubSub()
	require.NoError(t, err)
	defer cleanup()

	// Context with NO deadline
	ctx := context.Background()
	_, err = ps.Request(ctx, "test.no.responder", []byte("data"))
	require.ErrorIs(t, err, nats.ErrNoResponders)
}

func TestSystem_Stream_UnsubscribeStopsDelivery(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	ps, cleanup, err := libbus.NewTestPubSub()
	require.NoError(t, err)
	defer cleanup()

	subject := "test.unsubscribe"
	streamCh := make(chan []byte, 1)

	sub, err := ps.Stream(ctx, subject, streamCh)
	require.NoError(t, err)
	require.NoError(t, sub.Unsubscribe())

	// Publish AFTER unsubscribe
	require.NoError(t, ps.Publish(ctx, subject, []byte("unsubscribed")))

	// Should NOT receive message
	select {
	case <-streamCh:
		t.Fatal("received message after unsubscribe")
	case <-time.After(100 * time.Millisecond):
		// Expected: no message
	}
}

func TestSystem_Serve_ContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	ps, cleanup, err := libbus.NewTestPubSub()
	require.NoError(t, err)
	defer cleanup()

	subject := "test.serve.context"
	handlerCalled := false

	handler := func(ctx context.Context, data []byte) ([]byte, error) {
		handlerCalled = true
		return []byte("response"), nil
	}

	// Start serving
	_, err = ps.Serve(ctx, subject, handler)
	require.NoError(t, err)

	// Cancel context
	cancel()
	time.Sleep(100 * time.Millisecond) // Allow unsubscription

	// Request should fail (no responder)
	_, err = ps.Request(context.Background(), subject, []byte("request"))
	require.ErrorIs(t, err, nats.ErrNoResponders)
	require.False(t, handlerCalled, "handler should not be called after cancellation")
}

func TestSystem_Serve_HandlerPanic(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	ps, cleanup, err := libbus.NewTestPubSub()
	require.NoError(t, err)
	defer cleanup()

	subject := "test.handler.panic"
	panicMsg := "intentional panic"

	handler := func(ctx context.Context, data []byte) ([]byte, error) {
		panic(panicMsg)
	}

	// Start serving
	sub, err := ps.Serve(ctx, subject, handler)
	require.NoError(t, err)
	defer sub.Unsubscribe()

	// Send request
	reply, err := ps.Request(ctx, subject, []byte("request"))
	require.NoError(t, err)

	// Should get panic error in response (updated format)
	expected := fmt.Sprintf("error: handler panic: %s", panicMsg)
	require.Contains(t, string(reply), expected)
}

func TestSystem_Serve_ConcurrentUnsubscribe(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	ps, cleanup, err := libbus.NewTestPubSub()
	require.NoError(t, err)
	defer cleanup()

	subject := "test.concurrent"
	handler := func(ctx context.Context, data []byte) ([]byte, error) {
		return []byte("response"), nil
	}

	// Start serving
	sub, err := ps.Serve(ctx, subject, handler)
	require.NoError(t, err)

	// Concurrent unsubscribe and request
	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		time.Sleep(50 * time.Millisecond)
		require.NoError(t, sub.Unsubscribe())
	}()

	go func() {
		defer wg.Done()
		// Request might succeed or fail depending on timing
		_, _ = ps.Request(ctx, subject, []byte("data"))
	}()

	wg.Wait()
	// No panic/crash is success
}

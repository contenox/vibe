package libbus_test

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	libbus "github.com/contenox/contenox/internal/libbus"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
)

func TestSystem_Publish_ContextCanceled(t *testing.T) {
	testcontainers.SkipIfProviderIsNotHealthy(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	ps, cleanup, err := libbus.NewTestPubSub()
	require.NoError(t, err)
	defer cleanup()

	err = ps.Publish(ctx, "test.canceled", []byte("data"))
	require.ErrorIs(t, err, context.Canceled)
}

func TestSystem_Stream_ContextCanceledBeforeCall(t *testing.T) {
	testcontainers.SkipIfProviderIsNotHealthy(t)
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
	testcontainers.SkipIfProviderIsNotHealthy(t)
	ctx, cancel := context.WithCancel(context.Background())
	ps, cleanup, err := libbus.NewTestPubSub()
	require.NoError(t, err)
	defer cleanup()

	subject := "test.request.canceled"

	handler := func(ctx context.Context, data []byte) ([]byte, error) {
		time.Sleep(500 * time.Millisecond) // longer than the cancellation delay below
		return []byte("response"), nil
	}

	sub, err := ps.Serve(ctx, subject, handler)
	require.NoError(t, err)
	defer sub.Unsubscribe()

	errCh := make(chan error, 1)
	go func() {
		_, err := ps.Request(ctx, subject, []byte("data"))
		errCh <- err
	}()

	time.Sleep(100 * time.Millisecond) // ensure the request is in-flight before cancelling
	cancel()

	select {
	case err := <-errCh:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(1 * time.Second):
		t.Fatal("request didn't return after cancellation")
	}
}

func TestSystem_Stream_ConnectionClosed(t *testing.T) {
	testcontainers.SkipIfProviderIsNotHealthy(t)
	ps, cleanup, err := libbus.NewTestPubSub()
	require.NoError(t, err)
	require.NoError(t, ps.Close())
	cleanup()

	ch := make(chan []byte, 1)
	_, err = ps.Stream(context.Background(), "test.closed", ch)
	require.ErrorIs(t, err, libbus.ErrConnectionClosed)
}

func TestSystem_Serve_ConnectionClosed(t *testing.T) {
	testcontainers.SkipIfProviderIsNotHealthy(t)
	ps, cleanup, err := libbus.NewTestPubSub()
	require.NoError(t, err)
	require.NoError(t, ps.Close())
	cleanup()

	handler := func(ctx context.Context, data []byte) ([]byte, error) {
		return nil, nil
	}

	_, err = ps.Serve(context.Background(), "test.closed", handler)
	require.ErrorIs(t, err, libbus.ErrConnectionClosed)
}

func TestSystem_Request_NoResponder_NoDeadline(t *testing.T) {
	testcontainers.SkipIfProviderIsNotHealthy(t)
	ps, cleanup, err := libbus.NewTestPubSub()
	require.NoError(t, err)
	defer cleanup()

	ctx := context.Background()
	_, err = ps.Request(ctx, "test.no.responder", []byte("data"))
	require.ErrorIs(t, err, nats.ErrNoResponders)
}

func TestSystem_Stream_UnsubscribeStopsDelivery(t *testing.T) {
	testcontainers.SkipIfProviderIsNotHealthy(t)
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

	require.NoError(t, ps.Publish(ctx, subject, []byte("unsubscribed")))

	// Should NOT receive message
	select {
	case <-streamCh:
		t.Fatal("received message after unsubscribe")
	case <-time.After(100 * time.Millisecond):
	}
}

func TestSystem_Serve_ContextCancellation(t *testing.T) {
	testcontainers.SkipIfProviderIsNotHealthy(t)
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

	_, err = ps.Serve(ctx, subject, handler)
	require.NoError(t, err)

	cancel()
	time.Sleep(100 * time.Millisecond) // allow unsubscription to propagate

	_, err = ps.Request(context.Background(), subject, []byte("request"))
	require.ErrorIs(t, err, nats.ErrNoResponders)
	require.False(t, handlerCalled, "handler should not be called after cancellation")
}

func TestSystem_Serve_HandlerPanic(t *testing.T) {
	testcontainers.SkipIfProviderIsNotHealthy(t)
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

	sub, err := ps.Serve(ctx, subject, handler)
	require.NoError(t, err)
	defer sub.Unsubscribe()

	reply, err := ps.Request(ctx, subject, []byte("request"))
	require.NoError(t, err)

	expected := fmt.Sprintf("error: handler panic: %s", panicMsg)
	require.Contains(t, string(reply), expected)
}

func TestSystem_Serve_ConcurrentUnsubscribe(t *testing.T) {
	testcontainers.SkipIfProviderIsNotHealthy(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	ps, cleanup, err := libbus.NewTestPubSub()
	require.NoError(t, err)
	defer cleanup()

	subject := "test.concurrent"
	handler := func(ctx context.Context, data []byte) ([]byte, error) {
		return []byte("response"), nil
	}

	sub, err := ps.Serve(ctx, subject, handler)
	require.NoError(t, err)

	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		time.Sleep(50 * time.Millisecond)
		require.NoError(t, sub.Unsubscribe())
	}()

	go func() {
		defer wg.Done()
		// may succeed or fail depending on timing; only absence of a crash is asserted
		_, _ = ps.Request(ctx, subject, []byte("data"))
	}()

	wg.Wait()
}

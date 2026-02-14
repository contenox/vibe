package libbus_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	libbus "github.com/contenox/vibe/libbus"
	"github.com/stretchr/testify/require"
)

func TestSystem_Stream(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	ps, cleanup, err := libbus.NewTestPubSub()
	defer cleanup()
	if err != nil {
		t.Fatalf("failed to init test stream %s", err)
	}

	subject := "test.stream"
	message := []byte("streamed message")

	// Create a channel for streaming messages.
	streamCh := make(chan []byte, 1)
	sub, err := ps.Stream(ctx, subject, streamCh)
	require.NoError(t, err)
	defer sub.Unsubscribe()

	// Publish the message.
	err = ps.Publish(ctx, subject, message)
	require.NoError(t, err)

	// Wait for the streamed message.
	select {
	case received := <-streamCh:
		require.Equal(t, message, received)
	case <-ctx.Done():
		t.Fatal("timed out waiting for streamed message")
	}
}

func TestSystem_PublishWithClosedConnection(t *testing.T) {
	ctx := context.Background()

	ps, cleanup, err := libbus.NewTestPubSub()
	defer cleanup()
	if err != nil {
		t.Fatalf("failed to init test stream %s", err)
	}
	// Close the connection.
	err = ps.Close()
	require.NoError(t, err)

	// Attempt to publish after closing.
	err = ps.Publish(ctx, "test.closed", []byte("data"))
	require.Error(t, err)
	require.Equal(t, libbus.ErrConnectionClosed, err)
}

func TestSystem_RequestReply(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	ps, cleanup, err := libbus.NewTestPubSub()
	require.NoError(t, err)
	defer cleanup()

	subject := "test.request.reply"
	requestMessage := []byte("hello worker")
	responseMessage := []byte("hello client")

	// Define the worker handler.
	handler := func(ctx context.Context, data []byte) ([]byte, error) {
		require.Equal(t, requestMessage, data)
		return responseMessage, nil
	}

	// Start the worker to serve requests.
	sub, err := ps.Serve(ctx, subject, handler)
	require.NoError(t, err)
	defer sub.Unsubscribe()

	// Send a request and wait for the reply.
	reply, err := ps.Request(ctx, subject, requestMessage)
	require.NoError(t, err)
	require.Equal(t, responseMessage, reply)
}

func TestSystem_RequestReplyTimeout(t *testing.T) {
	ps, cleanup, err := libbus.NewTestPubSub()
	require.NoError(t, err)
	defer cleanup()

	subject := "test.request.timeout"

	// Create a context that times out immediately.
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Millisecond)
	defer cancel()

	// Send a request that is expected to time out.
	_, err = ps.Request(ctx, subject, []byte("should timeout"))
	require.Error(t, err)
	require.Equal(t, libbus.ErrRequestTimeout, err)
}

func TestSystem_ServeWithHandlerError(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	ps, cleanup, err := libbus.NewTestPubSub()
	require.NoError(t, err)
	defer cleanup()

	subject := "test.handler.error"
	requestMessage := []byte("this will fail")
	expectedError := "handler failed"

	// Define a worker handler that always returns an error.
	handler := func(ctx context.Context, data []byte) ([]byte, error) {
		return nil, errors.New(expectedError)
	}

	// Start the worker.
	sub, err := ps.Serve(ctx, subject, handler)
	require.NoError(t, err)
	defer sub.Unsubscribe()

	// Send a request.
	reply, err := ps.Request(ctx, subject, requestMessage)
	require.NoError(t, err) // The request itself doesn't fail, it gets a reply.

	// Check that the reply contains the error message.
	expectedReply := fmt.Appendf(nil, "error: %s", expectedError)
	require.Equal(t, expectedReply, reply)
}

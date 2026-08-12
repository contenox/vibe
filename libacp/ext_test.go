package libacp_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/contenox/contenox/libacp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func wireExtConnections(t *testing.T, ctx context.Context, configureAgent func(*libacp.AgentSideConnection), configureClient func(*libacp.ClientSideConnection)) (*libacp.AgentSideConnection, *libacp.ClientSideConnection, func()) {
	t.Helper()

	agentSide, clientSide := newPipePair()

	agentConn := libacp.NewAgentSideConnection(agentSide, func(c *libacp.AgentSideConnection) libacp.Agent {
		if configureAgent != nil {
			configureAgent(c)
		}
		return libacp.UnimplementedAgent{}
	})
	clientConn := libacp.NewClientSideConnection(clientSide, func(c *libacp.ClientSideConnection) libacp.Client {
		if configureClient != nil {
			configureClient(c)
		}
		return libacp.UnimplementedClient{}
	})

	agentRunErr := make(chan error, 1)
	go func() { agentRunErr <- agentConn.Run(ctx) }()
	clientRunErr := make(chan error, 1)
	go func() { clientRunErr <- clientConn.Run(ctx) }()

	cleanup := func() {
		_ = agentSide.Close()
		select {
		case <-agentRunErr:
		case <-time.After(time.Second):
			t.Error("agent connection did not shut down")
		}
		select {
		case <-clientRunErr:
		case <-time.After(time.Second):
			t.Error("client connection did not shut down")
		}
	}
	return agentConn, clientConn, cleanup
}

func TestUnit_AgentSide_ExtRequest_WireShape(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	agentSide, clientSide := newPipePair()
	conn := libacp.NewAgentSideConnection(agentSide, func(c *libacp.AgentSideConnection) libacp.Agent {
		c.SetExtRequestHandler(func(_ context.Context, method string, params json.RawMessage) (json.RawMessage, *libacp.Error) {
			assert.Equal(t, "_acme.dev/echo", method)
			assert.JSONEq(t, `{"greeting":"hi"}`, string(params))
			return json.RawMessage(`{"echoed":"hi"}`), nil
		})
		return libacp.UnimplementedAgent{}
	})
	go func() { _ = conn.Run(ctx) }()
	t.Cleanup(func() { _ = clientSide.Close() })

	write, read := bufWriter(clientSide), bufReader(clientSide)

	raw, err := json.Marshal(map[string]string{"greeting": "hi"})
	require.NoError(t, err)
	require.NoError(t, write(libacp.NewRequest(libacp.NewRequestIDNumber(1), "_acme.dev/echo", raw)))

	line, err := read()
	require.NoError(t, err)
	in, err := libacp.ParseIncoming(line)
	require.NoError(t, err)
	require.Equal(t, libacp.IncomingKindResponse, in.Kind, "wire: %s", line)
	require.Nil(t, in.Response.Error, "wire: %s", line)
	assert.JSONEq(t, `{"echoed":"hi"}`, string(in.Response.Result), "wire: %s", line)
}

func TestUnit_AgentSide_ExtRequest_NilHandler_MethodNotFound(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	agentSide, clientSide := newPipePair()
	conn := libacp.NewAgentSideConnection(agentSide, func(*libacp.AgentSideConnection) libacp.Agent {
		return libacp.UnimplementedAgent{}
	})
	go func() { _ = conn.Run(ctx) }()
	t.Cleanup(func() { _ = clientSide.Close() })

	write, read := bufWriter(clientSide), bufReader(clientSide)
	require.NoError(t, write(libacp.NewRequest(libacp.NewRequestIDNumber(1), "_acme.dev/echo", nil)))

	line, err := read()
	require.NoError(t, err)
	in, err := libacp.ParseIncoming(line)
	require.NoError(t, err)
	require.Equal(t, libacp.IncomingKindResponse, in.Kind, "wire: %s", line)
	require.NotNil(t, in.Response.Error, "a nil ext handler must preserve MethodNotFound, wire: %s", line)
	assert.Equal(t, libacp.ErrMethodNotFound, in.Response.Error.Code)
}

func TestUnit_AgentSide_ExtNotification_Delivered(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	seen := make(chan string, 1)
	agentSide, clientSide := newPipePair()
	conn := libacp.NewAgentSideConnection(agentSide, func(c *libacp.AgentSideConnection) libacp.Agent {
		c.SetExtNotificationHandler(func(_ context.Context, method string, params json.RawMessage) {
			seen <- method + ":" + string(params)
		})
		return libacp.UnimplementedAgent{}
	})
	go func() { _ = conn.Run(ctx) }()
	t.Cleanup(func() { _ = clientSide.Close() })

	raw, err := json.Marshal(map[string]int{"x": 1})
	require.NoError(t, err)
	require.NoError(t, bufWriter(clientSide)(libacp.NewNotification("_acme.dev/ping", raw)))

	select {
	case got := <-seen:
		assert.Equal(t, `_acme.dev/ping:{"x":1}`, got)
	case <-time.After(2 * time.Second):
		t.Fatal("extension notification handler never invoked")
	}
}

func TestUnit_AgentSide_UnknownNonExtensionMethod_StillMethodNotFound(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	agentSide, clientSide := newPipePair()
	conn := libacp.NewAgentSideConnection(agentSide, func(c *libacp.AgentSideConnection) libacp.Agent {
		// Catch-all handler that would wrongly answer any method if the "_" gate were bypassed.
		c.SetExtRequestHandler(func(context.Context, string, json.RawMessage) (json.RawMessage, *libacp.Error) {
			return json.RawMessage(`{}`), nil
		})
		return libacp.UnimplementedAgent{}
	})
	go func() { _ = conn.Run(ctx) }()
	t.Cleanup(func() { _ = clientSide.Close() })

	write, read := bufWriter(clientSide), bufReader(clientSide)
	require.NoError(t, write(libacp.NewRequest(libacp.NewRequestIDNumber(1), "totally/unknown", nil)))

	line, err := read()
	require.NoError(t, err)
	in, err := libacp.ParseIncoming(line)
	require.NoError(t, err)
	require.Equal(t, libacp.IncomingKindResponse, in.Kind, "wire: %s", line)
	require.NotNil(t, in.Response.Error, "a non-\"_\" method must never reach the ext handler, wire: %s", line)
	assert.Equal(t, libacp.ErrMethodNotFound, in.Response.Error.Code)
}

func TestUnit_ClientSide_ExtRequest_WireShape(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	clientSide, peerSide := newPipePair()
	conn := libacp.NewClientSideConnection(clientSide, func(c *libacp.ClientSideConnection) libacp.Client {
		c.SetExtRequestHandler(func(_ context.Context, method string, params json.RawMessage) (json.RawMessage, *libacp.Error) {
			assert.Equal(t, "_acme.dev/echo", method)
			assert.JSONEq(t, `{"greeting":"hi"}`, string(params))
			return json.RawMessage(`{"echoed":"hi"}`), nil
		})
		return libacp.UnimplementedClient{}
	})
	go func() { _ = conn.Run(ctx) }()
	t.Cleanup(func() { _ = peerSide.Close() })

	write, read := bufWriter(peerSide), bufReader(peerSide)

	raw, err := json.Marshal(map[string]string{"greeting": "hi"})
	require.NoError(t, err)
	require.NoError(t, write(libacp.NewRequest(libacp.NewRequestIDNumber(1), "_acme.dev/echo", raw)))

	line, err := read()
	require.NoError(t, err)
	in, err := libacp.ParseIncoming(line)
	require.NoError(t, err)
	require.Equal(t, libacp.IncomingKindResponse, in.Kind, "wire: %s", line)
	require.Nil(t, in.Response.Error, "wire: %s", line)
	assert.JSONEq(t, `{"echoed":"hi"}`, string(in.Response.Result), "wire: %s", line)
}

func TestUnit_ClientSide_ExtRequest_NilHandler_MethodNotFound(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	clientSide, peerSide := newPipePair()
	conn := libacp.NewClientSideConnection(clientSide, func(*libacp.ClientSideConnection) libacp.Client {
		return libacp.UnimplementedClient{}
	})
	go func() { _ = conn.Run(ctx) }()
	t.Cleanup(func() { _ = peerSide.Close() })

	write, read := bufWriter(peerSide), bufReader(peerSide)
	require.NoError(t, write(libacp.NewRequest(libacp.NewRequestIDNumber(1), "_acme.dev/echo", nil)))

	line, err := read()
	require.NoError(t, err)
	in, err := libacp.ParseIncoming(line)
	require.NoError(t, err)
	require.Equal(t, libacp.IncomingKindResponse, in.Kind, "wire: %s", line)
	require.NotNil(t, in.Response.Error, "a nil ext handler must preserve MethodNotFound, wire: %s", line)
	assert.Equal(t, libacp.ErrMethodNotFound, in.Response.Error.Code)
}

func TestUnit_ClientSide_ExtNotification_Delivered(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	seen := make(chan string, 1)
	clientSide, peerSide := newPipePair()
	conn := libacp.NewClientSideConnection(clientSide, func(c *libacp.ClientSideConnection) libacp.Client {
		c.SetExtNotificationHandler(func(_ context.Context, method string, params json.RawMessage) {
			seen <- method + ":" + string(params)
		})
		return libacp.UnimplementedClient{}
	})
	go func() { _ = conn.Run(ctx) }()
	t.Cleanup(func() { _ = peerSide.Close() })

	raw, err := json.Marshal(map[string]int{"x": 1})
	require.NoError(t, err)
	require.NoError(t, bufWriter(peerSide)(libacp.NewNotification("_acme.dev/ping", raw)))

	select {
	case got := <-seen:
		assert.Equal(t, `_acme.dev/ping:{"x":1}`, got)
	case <-time.After(2 * time.Second):
		t.Fatal("extension notification handler never invoked")
	}
}

func TestUnit_ClientSide_UnknownNonExtensionMethod_StillMethodNotFound(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	clientSide, peerSide := newPipePair()
	conn := libacp.NewClientSideConnection(clientSide, func(c *libacp.ClientSideConnection) libacp.Client {
		c.SetExtRequestHandler(func(context.Context, string, json.RawMessage) (json.RawMessage, *libacp.Error) {
			return json.RawMessage(`{}`), nil
		})
		return libacp.UnimplementedClient{}
	})
	go func() { _ = conn.Run(ctx) }()
	t.Cleanup(func() { _ = peerSide.Close() })

	write, read := bufWriter(peerSide), bufReader(peerSide)
	require.NoError(t, write(libacp.NewRequest(libacp.NewRequestIDNumber(1), "totally/unknown", nil)))

	line, err := read()
	require.NoError(t, err)
	in, err := libacp.ParseIncoming(line)
	require.NoError(t, err)
	require.Equal(t, libacp.IncomingKindResponse, in.Kind, "wire: %s", line)
	require.NotNil(t, in.Response.Error, "a non-\"_\" method must never reach the ext handler, wire: %s", line)
	assert.Equal(t, libacp.ErrMethodNotFound, in.Response.Error.Code)
}

func TestUnit_ExtRequest_AgentToClient_Loopback(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	agentConn, _, cleanup := wireExtConnections(t, ctx, nil, func(c *libacp.ClientSideConnection) {
		c.SetExtRequestHandler(func(_ context.Context, method string, params json.RawMessage) (json.RawMessage, *libacp.Error) {
			assert.Equal(t, "_acme.dev/echo", method)
			assert.JSONEq(t, `{"n":1}`, string(params))
			return json.RawMessage(`{"n":2}`), nil
		})
	})
	defer cleanup()

	result, err := agentConn.CallExtMethod(ctx, "_acme.dev/echo", json.RawMessage(`{"n":1}`))
	require.NoError(t, err)
	assert.JSONEq(t, `{"n":2}`, string(result))
}

func TestUnit_ExtRequest_ClientToAgent_Loopback(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, clientConn, cleanup := wireExtConnections(t, ctx, func(c *libacp.AgentSideConnection) {
		c.SetExtRequestHandler(func(_ context.Context, method string, params json.RawMessage) (json.RawMessage, *libacp.Error) {
			assert.Equal(t, "_acme.dev/echo", method)
			assert.JSONEq(t, `{"n":1}`, string(params))
			return json.RawMessage(`{"n":2}`), nil
		})
	}, nil)
	defer cleanup()

	result, err := clientConn.CallExtMethod(ctx, "_acme.dev/echo", json.RawMessage(`{"n":1}`))
	require.NoError(t, err)
	assert.JSONEq(t, `{"n":2}`, string(result))
}

func TestUnit_ExtNotification_AgentToClient_Loopback(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	seen := make(chan string, 1)
	agentConn, _, cleanup := wireExtConnections(t, ctx, nil, func(c *libacp.ClientSideConnection) {
		c.SetExtNotificationHandler(func(_ context.Context, method string, _ json.RawMessage) {
			seen <- method
		})
	})
	defer cleanup()

	require.NoError(t, agentConn.SendExtNotification("_acme.dev/ping", nil))
	select {
	case got := <-seen:
		assert.Equal(t, "_acme.dev/ping", got)
	case <-time.After(2 * time.Second):
		t.Fatal("extension notification never reached the client")
	}
}

func TestUnit_ExtNotification_ClientToAgent_Loopback(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	seen := make(chan string, 1)
	_, clientConn, cleanup := wireExtConnections(t, ctx, func(c *libacp.AgentSideConnection) {
		c.SetExtNotificationHandler(func(_ context.Context, method string, _ json.RawMessage) {
			seen <- method
		})
	}, nil)
	defer cleanup()

	require.NoError(t, clientConn.SendExtNotification("_acme.dev/ping", nil))
	select {
	case got := <-seen:
		assert.Equal(t, "_acme.dev/ping", got)
	case <-time.After(2 * time.Second):
		t.Fatal("extension notification never reached the agent")
	}
}

// Invariant: an in-flight extension request participates in
// "$/cancel_request" cancellation exactly like a core method's handler.
func TestUnit_ExtRequest_CancelRequest_AbortsInFlightHandler(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	started := make(chan struct{})
	handlerCancelled := make(chan struct{})
	agentConn, _, cleanup := wireExtConnections(t, ctx, nil, func(c *libacp.ClientSideConnection) {
		c.SetExtRequestHandler(func(ctx context.Context, _ string, _ json.RawMessage) (json.RawMessage, *libacp.Error) {
			close(started)
			<-ctx.Done()
			close(handlerCancelled)
			return nil, libacp.NewError(libacp.ErrInternalError, "aborted")
		})
	})
	defer cleanup()

	callCtx, callCancel := context.WithCancel(ctx)
	resultErr := make(chan error, 1)
	go func() {
		_, err := agentConn.CallExtMethod(callCtx, "_acme.dev/slow", nil)
		resultErr <- err
	}()

	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("ext handler never started")
	}

	callCancel()

	select {
	case err := <-resultErr:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(2 * time.Second):
		t.Fatal("CallExtMethod never returned after its context was cancelled")
	}

	select {
	case <-handlerCancelled:
	case <-time.After(2 * time.Second):
		t.Fatal("$/cancel_request never reached the in-flight ext handler")
	}
}

func TestUnit_CallExtMethod_And_SendExtNotification_RejectNonExtensionMethodName(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	agentConn, clientConn, cleanup := wireExtConnections(t, ctx, nil, nil)
	defer cleanup()

	_, err := agentConn.CallExtMethod(ctx, "session/prompt", nil)
	assert.Error(t, err)
	assert.Error(t, agentConn.SendExtNotification("session/update", nil))

	_, err = clientConn.CallExtMethod(ctx, "initialize", nil)
	assert.Error(t, err)
	assert.Error(t, clientConn.SendExtNotification("$/cancel_request", nil))
}

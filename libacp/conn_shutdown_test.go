package libacp_test

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"
	"time"

	"github.com/contenox/beam/libacp"
	"github.com/stretchr/testify/require"
)

// handlerJoinDelay is how long the slow handlers below keep working after
// their context is cancelled, long enough that a Run which fails to join its
// handler goroutines loses the race deterministically.
const handlerJoinDelay = 250 * time.Millisecond

// lingeringAgent's NewSession keeps running for handlerJoinDelay after its
// context is cancelled, then records that it has returned (the strong
// property beyond merely observing cancellation).
type lingeringAgent struct {
	libacp.UnimplementedAgent
	entered  chan struct{}
	returned atomic.Bool
}

func (a *lingeringAgent) Initialize(_ context.Context, _ libacp.InitializeRequest) (libacp.InitializeResponse, error) {
	return libacp.InitializeResponse{ProtocolVersion: libacp.ProtocolVersion}, nil
}

func (a *lingeringAgent) NewSession(ctx context.Context, _ libacp.NewSessionRequest) (libacp.NewSessionResponse, error) {
	close(a.entered)
	<-ctx.Done()
	time.Sleep(handlerJoinDelay)
	a.returned.Store(true)
	return libacp.NewSessionResponse{SessionID: "sess-1"}, nil
}

// Invariant: Run must not return while an inbound request handler is still
// executing, since the caller tears down live session state the instant it does.
func TestUnit_AgentSideRun_JoinsInFlightRequestHandlerBeforeReturning(t *testing.T) {
	agent := &lingeringAgent{entered: make(chan struct{})}

	agentSide, clientSide := newPipePair()
	t.Cleanup(func() { _ = clientSide.Close() })
	conn := libacp.NewAgentSideConnection(agentSide, func(*libacp.AgentSideConnection) libacp.Agent { return agent })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runErr := make(chan error, 1)
	go func() { runErr <- conn.Run(ctx) }()

	write := bufWriter(clientSide)
	params, err := json.Marshal(libacp.NewSessionRequest{Cwd: "/tmp"})
	require.NoError(t, err)
	require.NoError(t, write(libacp.NewRequest(libacp.NewRequestIDNumber(1), libacp.MethodSessionNew, params)))

	select {
	case <-agent.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("NewSession handler never started")
	}

	cancel()

	select {
	case <-runErr:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after ctx cancellation")
	}
	require.True(t, agent.returned.Load(),
		"Run returned while the NewSession handler goroutine was still executing")
}

// lingeringClient is the client-side mirror of lingeringAgent.
type lingeringClient struct {
	libacp.UnimplementedClient
	entered  chan struct{}
	returned atomic.Bool
}

func (c *lingeringClient) ReadTextFile(ctx context.Context, _ libacp.ReadTextFileRequest) (libacp.ReadTextFileResponse, error) {
	close(c.entered)
	<-ctx.Done()
	time.Sleep(handlerJoinDelay)
	c.returned.Store(true)
	return libacp.ReadTextFileResponse{}, ctx.Err()
}

// Same invariant on the editor side: ClientSideConnection.Run joins its
// inbound (agent->client) request handlers before returning.
func TestUnit_ClientSideRun_JoinsInFlightRequestHandlerBeforeReturning(t *testing.T) {
	client := &lingeringClient{entered: make(chan struct{})}

	clientSide, agentSide := newPipePair()
	t.Cleanup(func() { _ = agentSide.Close() })
	conn := libacp.NewClientSideConnection(clientSide, func(*libacp.ClientSideConnection) libacp.Client { return client })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runErr := make(chan error, 1)
	go func() { runErr <- conn.Run(ctx) }()

	write := bufWriter(agentSide)
	params, err := json.Marshal(libacp.ReadTextFileRequest{SessionID: "sess-1", Path: "/tmp/x.txt"})
	require.NoError(t, err)
	require.NoError(t, write(libacp.NewRequest(libacp.NewRequestIDNumber(1), libacp.MethodFSReadTextFile, params)))

	select {
	case <-client.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("ReadTextFile handler never started")
	}

	cancel()

	select {
	case <-runErr:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after ctx cancellation")
	}
	require.True(t, client.returned.Load(),
		"Run returned while the ReadTextFile handler goroutine was still executing")
}

// lingeringNotifyAgent's Cancel lingers past cancellation the same way,
// pinning that notification dispatch is joined too, not just requests.
type lingeringNotifyAgent struct {
	libacp.UnimplementedAgent
	entered  chan struct{}
	returned atomic.Bool
}

func (a *lingeringNotifyAgent) Cancel(_ context.Context, _ libacp.CancelNotification) error {
	close(a.entered)
	time.Sleep(handlerJoinDelay)
	a.returned.Store(true)
	return nil
}

func TestUnit_AgentSideRun_JoinsInFlightNotificationHandlerBeforeReturning(t *testing.T) {
	agent := &lingeringNotifyAgent{entered: make(chan struct{})}

	agentSide, clientSide := newPipePair()
	t.Cleanup(func() { _ = clientSide.Close() })
	conn := libacp.NewAgentSideConnection(agentSide, func(*libacp.AgentSideConnection) libacp.Agent { return agent })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runErr := make(chan error, 1)
	go func() { runErr <- conn.Run(ctx) }()

	write := bufWriter(clientSide)
	params, err := json.Marshal(libacp.CancelNotification{SessionID: "sess-1"})
	require.NoError(t, err)
	require.NoError(t, write(libacp.NewNotification(libacp.MethodSessionCancel, params)))

	select {
	case <-agent.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("Cancel notification handler never started")
	}

	cancel()

	select {
	case <-runErr:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after ctx cancellation")
	}
	require.True(t, agent.returned.Load(),
		"Run returned while the session/cancel notification handler was still executing")
}

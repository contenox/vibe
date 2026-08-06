package libacp_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/contenox/contenox/libacp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// testAgent is a configurable libacp.Agent that plays the agent side of the
// wire so a real ClientSideConnection can be exercised end to end.
type testAgent struct {
	libacp.UnimplementedAgent

	conn *libacp.AgentSideConnection

	mu         sync.Mutex
	promptReq  libacp.PromptRequest
	promptErr  *libacp.Error
	cancelSeen chan libacp.CancelNotification

	// When set, Prompt calls back into the client (fs/read_text_file,
	// session/request_permission) instead of streaming session/update chunks.
	callClientRPCs bool
	rpcResult      struct {
		readResp libacp.ReadTextFileResponse
		readErr  error
		permResp libacp.RequestPermissionResponse
		permErr  error
	}
}

func (a *testAgent) Initialize(_ context.Context, req libacp.InitializeRequest) (libacp.InitializeResponse, error) {
	return libacp.InitializeResponse{
		ProtocolVersion: libacp.ProtocolVersion,
		AgentInfo:       &libacp.Implementation{Name: "test-agent", Version: "1.0.0"},
		AgentCapabilities: libacp.AgentCapabilities{
			PromptCapabilities: libacp.PromptCapabilities{
				EmbeddedContext: req.ClientCapabilities.FS.ReadTextFile,
			},
		},
	}, nil
}

func (a *testAgent) NewSession(_ context.Context, _ libacp.NewSessionRequest) (libacp.NewSessionResponse, error) {
	return libacp.NewSessionResponse{SessionID: libacp.SessionID("sess-1")}, nil
}

func (a *testAgent) Prompt(ctx context.Context, req libacp.PromptRequest) (libacp.PromptResponse, error) {
	a.mu.Lock()
	a.promptReq = req
	promptErr := a.promptErr
	callRPCs := a.callClientRPCs
	a.mu.Unlock()

	if promptErr != nil {
		return libacp.PromptResponse{}, promptErr
	}

	if callRPCs {
		readResp, readErr := a.conn.ReadTextFile(ctx, libacp.ReadTextFileRequest{
			SessionID: req.SessionID,
			Path:      "/tmp/x.txt",
		})
		permResp, permErr := a.conn.RequestPermission(ctx, libacp.RequestPermissionRequest{
			SessionID: req.SessionID,
			ToolCall:  libacp.PermissionToolCall{ToolCallID: "tc-1"},
			Options: []libacp.PermissionOption{
				{OptionID: "allow", Name: "Allow", Kind: libacp.PermissionAllowOnce},
			},
		})
		a.mu.Lock()
		a.rpcResult.readResp, a.rpcResult.readErr = readResp, readErr
		a.rpcResult.permResp, a.rpcResult.permErr = permResp, permErr
		a.mu.Unlock()
		return libacp.PromptResponse{StopReason: libacp.StopReasonEndTurn}, nil
	}

	if err := a.conn.SessionUpdate(libacp.SessionNotification{
		SessionID: req.SessionID,
		Update:    libacp.NewAgentMessageChunk("hello "),
	}); err != nil {
		return libacp.PromptResponse{}, err
	}
	if err := a.conn.SessionUpdate(libacp.SessionNotification{
		SessionID: req.SessionID,
		Update:    libacp.NewAgentMessageChunk("world"),
	}); err != nil {
		return libacp.PromptResponse{}, err
	}
	return libacp.PromptResponse{StopReason: libacp.StopReasonEndTurn}, nil
}

func (a *testAgent) Cancel(_ context.Context, req libacp.CancelNotification) error {
	if a.cancelSeen != nil {
		a.cancelSeen <- req
	}
	return nil
}

// testClient is a configurable libacp.Client used to answer agent->client
// requests and to record session/update notifications delivered to
// SessionUpdate, in the order they were received.
type testClient struct {
	libacp.UnimplementedClient

	mu      sync.Mutex
	updates []libacp.SessionNotification

	readResp libacp.ReadTextFileResponse
	permResp libacp.RequestPermissionResponse
}

func (c *testClient) SessionUpdate(_ context.Context, n libacp.SessionNotification) error {
	c.mu.Lock()
	c.updates = append(c.updates, n)
	c.mu.Unlock()
	return nil
}

func (c *testClient) ReadTextFile(_ context.Context, _ libacp.ReadTextFileRequest) (libacp.ReadTextFileResponse, error) {
	return c.readResp, nil
}

func (c *testClient) RequestPermission(_ context.Context, _ libacp.RequestPermissionRequest) (libacp.RequestPermissionResponse, error) {
	return c.permResp, nil
}

// wireUpTestConnections starts an AgentSideConnection and a ClientSideConnection
// back to back over an in-memory pipe, each running its own Run loop, and
// returns a cleanup func that closes the pipe and waits for both to exit.
func wireUpTestConnections(t *testing.T, ctx context.Context, agent *testAgent, client libacp.Client) (*libacp.AgentSideConnection, *libacp.ClientSideConnection, func()) {
	t.Helper()

	agentSide, clientSide := newPipePair()

	agentConn := libacp.NewAgentSideConnection(agentSide, func(c *libacp.AgentSideConnection) libacp.Agent {
		agent.conn = c
		return agent
	})

	clientConn := libacp.NewClientSideConnection(clientSide, func(*libacp.ClientSideConnection) libacp.Client {
		return client
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

func TestUnit_ClientSideConnection_InitializeSessionPrompt(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	agent := &testAgent{}
	client := &testClient{}
	_, clientConn, cleanup := wireUpTestConnections(t, ctx, agent, client)
	defer cleanup()

	initResp, err := clientConn.Initialize(ctx, libacp.InitializeRequest{
		ProtocolVersion: libacp.ProtocolVersion,
		ClientCapabilities: libacp.ClientCapabilities{
			FS: libacp.FileSystemCapabilities{ReadTextFile: true},
		},
	})
	require.NoError(t, err)
	assert.Equal(t, libacp.ProtocolVersion, initResp.ProtocolVersion)
	require.NotNil(t, initResp.AgentInfo)
	assert.Equal(t, "test-agent", initResp.AgentInfo.Name)
	assert.True(t, initResp.AgentCapabilities.PromptCapabilities.EmbeddedContext)

	newSessResp, err := clientConn.NewSession(ctx, libacp.NewSessionRequest{
		Cwd:        "/tmp",
		McpServers: []libacp.McpServer{},
	})
	require.NoError(t, err)
	assert.Equal(t, libacp.SessionID("sess-1"), newSessResp.SessionID)

	promptResp, err := clientConn.Prompt(ctx, libacp.PromptRequest{
		SessionID: newSessResp.SessionID,
		Prompt:    []libacp.ContentBlock{libacp.NewTextContent("say hi")},
	})
	require.NoError(t, err)
	assert.Equal(t, libacp.StopReasonEndTurn, promptResp.StopReason)

	// Both session/update chunks must be delivered, in order, before Prompt's response.
	client.mu.Lock()
	updates := append([]libacp.SessionNotification(nil), client.updates...)
	client.mu.Unlock()
	require.Len(t, updates, 2)
	require.NotNil(t, updates[0].Update.Content)
	assert.Equal(t, "hello ", updates[0].Update.Content.Text)
	require.NotNil(t, updates[1].Update.Content)
	assert.Equal(t, "world", updates[1].Update.Content.Text)

	agent.mu.Lock()
	assert.Equal(t, libacp.SessionID("sess-1"), agent.promptReq.SessionID)
	agent.mu.Unlock()
}

func TestUnit_ClientSideConnection_ServesAgentInitiatedRequests(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	agent := &testAgent{callClientRPCs: true}
	client := &testClient{
		readResp: libacp.ReadTextFileResponse{Content: "file contents"},
		permResp: libacp.RequestPermissionResponse{
			Outcome: libacp.RequestPermissionOutcome{
				Outcome:  libacp.PermissionOutcomeSelected,
				OptionID: "allow",
			},
		},
	}
	_, clientConn, cleanup := wireUpTestConnections(t, ctx, agent, client)
	defer cleanup()

	_, err := clientConn.Initialize(ctx, libacp.InitializeRequest{ProtocolVersion: libacp.ProtocolVersion})
	require.NoError(t, err)
	newSessResp, err := clientConn.NewSession(ctx, libacp.NewSessionRequest{Cwd: "/tmp", McpServers: []libacp.McpServer{}})
	require.NoError(t, err)

	promptResp, err := clientConn.Prompt(ctx, libacp.PromptRequest{
		SessionID: newSessResp.SessionID,
		Prompt:    []libacp.ContentBlock{libacp.NewTextContent("do the thing")},
	})
	require.NoError(t, err)
	assert.Equal(t, libacp.StopReasonEndTurn, promptResp.StopReason)

	agent.mu.Lock()
	defer agent.mu.Unlock()
	require.NoError(t, agent.rpcResult.readErr)
	assert.Equal(t, "file contents", agent.rpcResult.readResp.Content)
	require.NoError(t, agent.rpcResult.permErr)
	assert.Equal(t, libacp.PermissionOutcomeSelected, agent.rpcResult.permResp.Outcome.Outcome)
	assert.Equal(t, "allow", agent.rpcResult.permResp.Outcome.OptionID)
}

func TestUnit_ClientSideConnection_PromptErrorPropagation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	wantErr := libacp.NewError(libacp.ErrInvalidParams, "bad prompt")
	agent := &testAgent{promptErr: wantErr}
	client := &testClient{}
	_, clientConn, cleanup := wireUpTestConnections(t, ctx, agent, client)
	defer cleanup()

	_, err := clientConn.Initialize(ctx, libacp.InitializeRequest{ProtocolVersion: libacp.ProtocolVersion})
	require.NoError(t, err)
	newSessResp, err := clientConn.NewSession(ctx, libacp.NewSessionRequest{Cwd: "/tmp", McpServers: []libacp.McpServer{}})
	require.NoError(t, err)

	_, err = clientConn.Prompt(ctx, libacp.PromptRequest{
		SessionID: newSessResp.SessionID,
		Prompt:    []libacp.ContentBlock{libacp.NewTextContent("fail please")},
	})
	require.Error(t, err)
	var rpcErr *libacp.Error
	require.True(t, errors.As(err, &rpcErr))
	assert.Equal(t, libacp.ErrInvalidParams, rpcErr.Code)
	assert.Equal(t, "bad prompt", rpcErr.Message)
}

func TestUnit_ClientSideConnection_CancelSessionNotificationReachesAgent(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	cancelSeen := make(chan libacp.CancelNotification, 1)
	agent := &testAgent{cancelSeen: cancelSeen}
	client := &testClient{}
	_, clientConn, cleanup := wireUpTestConnections(t, ctx, agent, client)
	defer cleanup()

	_, err := clientConn.Initialize(ctx, libacp.InitializeRequest{ProtocolVersion: libacp.ProtocolVersion})
	require.NoError(t, err)
	newSessResp, err := clientConn.NewSession(ctx, libacp.NewSessionRequest{Cwd: "/tmp", McpServers: []libacp.McpServer{}})
	require.NoError(t, err)

	require.NoError(t, clientConn.CancelSession(libacp.CancelNotification{SessionID: newSessResp.SessionID}))

	select {
	case got := <-cancelSeen:
		assert.Equal(t, newSessResp.SessionID, got.SessionID)
	case <-time.After(2 * time.Second):
		t.Fatal("agent did not receive session/cancel notification")
	}
}

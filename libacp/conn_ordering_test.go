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

// cmdStubAgent advertises an available-commands update while handling
// session/new, scheduled via AfterResponse so it is written after the result.
type cmdStubAgent struct {
	libacp.UnimplementedAgent
	conn *libacp.AgentSideConnection
}

func (a *cmdStubAgent) Initialize(_ context.Context, _ libacp.InitializeRequest) (libacp.InitializeResponse, error) {
	return libacp.InitializeResponse{
		ProtocolVersion: libacp.ProtocolVersion,
		AgentInfo:       &libacp.Implementation{Name: "cmd-stub", Version: "0.0.1"},
	}, nil
}

func (a *cmdStubAgent) NewSession(ctx context.Context, _ libacp.NewSessionRequest) (libacp.NewSessionResponse, error) {
	libacp.AfterResponse(ctx, func() {
		_ = a.conn.SessionUpdate(libacp.SessionNotification{
			SessionID: libacp.SessionID("sess-1"),
			Update: libacp.SessionUpdate{
				SessionUpdate:     libacp.SessionUpdateAvailableCommands,
				AvailableCommands: []libacp.AvailableCommand{{Name: "help", Description: "List the available commands."}},
			},
		})
	})
	return libacp.NewSessionResponse{SessionID: libacp.SessionID("sess-1")}, nil
}

// Invariant: an available_commands_update emitted while handling session/new
// reaches the wire after the result, never before — a client learns a new
// session's id only from that result and drops an earlier update as
// referencing an unknown session.
func TestUnit_AgentSideConnection_AvailableCommandsAfterNewSessionResult(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	agentSide, clientSide := newPipePair()

	stub := &cmdStubAgent{}
	conn := libacp.NewAgentSideConnection(agentSide, func(c *libacp.AgentSideConnection) libacp.Agent {
		stub.conn = c
		return stub
	})

	runErr := make(chan error, 1)
	go func() { runErr <- conn.Run(ctx) }()

	clientReader := bufReader(clientSide)
	clientWriter := bufWriter(clientSide)

	send := func(method string, id int64, params any) {
		paramsRaw, err := json.Marshal(params)
		require.NoError(t, err)
		require.NoError(t, clientWriter(libacp.NewRequest(libacp.NewRequestIDNumber(id), method, paramsRaw)))
	}

	nextMessage := func() libacp.Incoming {
		line, err := clientReader()
		require.NoError(t, err)
		in, err := libacp.ParseIncoming(line)
		require.NoError(t, err)
		return in
	}

	send(libacp.MethodInitialize, 1, libacp.InitializeRequest{ProtocolVersion: libacp.ProtocolVersion})
	init := nextMessage()
	require.Equal(t, libacp.IncomingKindResponse, init.Kind)

	send(libacp.MethodSessionNew, 2, libacp.NewSessionRequest{Cwd: "/tmp", McpServers: []libacp.McpServer{}})

	// First message must be the result carrying the new session id.
	first := nextMessage()
	require.Equal(t, libacp.IncomingKindResponse, first.Kind,
		"session/new result must reach the client before any session/update for that session, "+
			"or the client drops the update as referencing an unknown session")
	require.Equal(t, libacp.NewRequestIDNumber(2), first.Response.ID)
	require.Nil(t, first.Response.Error)
	var newSess libacp.NewSessionResponse
	require.NoError(t, json.Unmarshal(first.Response.Result, &newSess))
	require.Equal(t, libacp.SessionID("sess-1"), newSess.SessionID)

	// The available_commands_update follows, now that the client knows the session.
	second := nextMessage()
	require.Equal(t, libacp.IncomingKindNotification, second.Kind)
	require.Equal(t, libacp.MethodSessionUpdate, second.Notification.Method)
	var sn libacp.SessionNotification
	require.NoError(t, json.Unmarshal(second.Notification.Params, &sn))
	assert.Equal(t, libacp.SessionUpdateAvailableCommands, sn.Update.SessionUpdate)
	require.Len(t, sn.Update.AvailableCommands, 1)
	assert.Equal(t, "help", sn.Update.AvailableCommands[0].Name)

	_ = clientSide.Close()
	select {
	case <-runErr:
	case <-time.After(time.Second):
		t.Fatal("connection did not shut down after client close")
	}
}

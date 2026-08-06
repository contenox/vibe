package acpsvc

import (
	"context"
	"strings"
	"testing"

	"github.com/contenox/contenox/internal/kernel/enginesvc"

	libacp "github.com/contenox/contenox/libacp"
	"github.com/stretchr/testify/require"
)

// TestLoopback_NewAndSessionsCommands_WorkWithoutASessionUI pins the core half
// of session management: an editor with no session UI of its own can start a
// second session and see the roster, both over an ordinary prompt. /new
// reports the id the client must session/load — ACP has no agent→client switch
// — and /sessions marks which one the client is on.
func TestLoopback_NewAndSessionsCommands_WorkWithoutASessionUI(t *testing.T) {
	h := newLoopbackHarness(t)
	ctx := context.Background()
	cwd := t.TempDir()

	_, err := h.client.Initialize(ctx, libacp.InitializeRequest{ProtocolVersion: libacp.ProtocolVersion})
	require.NoError(t, err)
	first, err := h.client.NewSession(ctx, libacp.NewSessionRequest{Cwd: cwd, McpServers: []libacp.McpServer{}})
	require.NoError(t, err)
	h.lc.drain(t, 1) // deferred available_commands_update

	resp, err := h.client.Prompt(ctx, libacp.PromptRequest{
		SessionID: first.SessionID,
		Prompt:    []libacp.ContentBlock{libacp.NewTextContent("/new")},
	})
	require.NoError(t, err)
	require.Equal(t, libacp.StopReasonEndTurn, resp.StopReason)

	created := waitForAgentMessage(t, h, first.SessionID, "Started session")
	require.Contains(t, created, cwd)
	require.Contains(t, created, "session/load", "the client is told how to open what it cannot be switched to")

	// The new session is a real, listed session — not a label.
	listed, err := h.client.ListSessions(ctx, libacp.ListSessionsRequest{Cwd: cwd})
	require.NoError(t, err)
	require.Len(t, listed.Sessions, 2, "/new must mint a durable session session/list can see")

	_, err = h.client.Prompt(ctx, libacp.PromptRequest{
		SessionID: first.SessionID,
		Prompt:    []libacp.ContentBlock{libacp.NewTextContent("/sessions")},
	})
	require.NoError(t, err)

	roster := waitForAgentMessage(t, h, first.SessionID, "Sessions in")
	for _, info := range listed.Sessions {
		require.Contains(t, roster, string(info.SessionID))
	}
	require.Contains(t, roster, "this session", "the roster must say which one the client is on")
	require.True(t, strings.Contains(roster, "* "+string(first.SessionID)), "the current session is the marked row")
}

// TestUnit_HandleSessions_EmptyWorkspaceTeachesNew pins the empty state: a
// workspace with nothing recorded names the one command that changes that.
func TestUnit_HandleSessions_EmptyWorkspaceTeachesNew(t *testing.T) {
	tr, _ := newAnswerTestTransport(t, nil, nil)
	out, err := tr.handleSessions(context.Background(), &sessionEntry{Cwd: t.TempDir()})
	require.NoError(t, err)
	require.Contains(t, out, "/new")
}

// TestUnit_HandleNewSessionCommand_RefusesWithoutAWorkspace pins the two
// refusals /new gives instead of minting a session it cannot honour: an
// unconfigured runtime gets the setup instruction, and a session with no
// workspace directory is told so rather than landing one in an unknown place.
func TestUnit_HandleNewSessionCommand_RefusesWithoutAWorkspace(t *testing.T) {
	tr, _ := newAnswerTestTransport(t, nil, nil)

	_, err := tr.handleNewSessionCommand(context.Background(), &sessionEntry{Cwd: t.TempDir()})
	require.Error(t, err, "an unconfigured runtime cannot start a session")
	require.Contains(t, err.Error(), "not configured yet")

	tr.deps.Engine = &enginesvc.Engine{}
	_, err = tr.handleNewSessionCommand(context.Background(), &sessionEntry{})
	require.Error(t, err)
	require.Contains(t, err.Error(), "workspace")
}

package acpsvc

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/contenox/contenox/internal/services/agentservice"
	libacp "github.com/contenox/contenox/libacp"
	"github.com/stretchr/testify/require"
)

// TestUnit_ExplainStopReason_EveryShortEndingHasASentence pins that no
// non-end_turn stop reason travels as a bare token: each carries what happened
// and, where one exists, the command that resolves it. end_turn is the
// ordinary ending and must stay unexplained.
func TestUnit_ExplainStopReason_EveryShortEndingHasASentence(t *testing.T) {
	for _, r := range []libacp.StopReason{
		libacp.StopReasonMaxTokens,
		libacp.StopReasonMaxTurnRequests,
		libacp.StopReasonRefusal,
		libacp.StopReasonCancelled,
	} {
		explained, ok := explainStopReason(r)
		require.True(t, ok, "%s must be explained", r)
		require.Equal(t, string(r), explained.Reason)
		require.NotEmpty(t, explained.Explanation)
		require.NotContains(t, explained.Explanation, "_", "the sentence must not echo the wire token")
	}

	_, ok := explainStopReason(libacp.StopReasonEndTurn)
	require.False(t, ok, "an ordinary ending needs no explanation")

	maxTokens, _ := explainStopReason(libacp.StopReasonMaxTokens)
	require.Contains(t, maxTokens.Command, "/max-tokens", "the token cap is raised by /max-tokens")
	require.Contains(t, stopReasonMessage(maxTokens), "/max-tokens")

	// A cancellation is the operator's own act: explained on the wire, never
	// announced back into the conversation.
	require.False(t, stopReasonAnnounced(libacp.StopReasonCancelled))
	require.True(t, stopReasonAnnounced(libacp.StopReasonMaxTokens))
}

// TestUnit_ExplainTurnStop_AttachesMetaAndLeavesExternalAlone pins the two
// rules at the transport boundary: a native turn's short ending rides back on
// the response `_meta`, and an external session is left as its downstream
// agent reported it (these commands are not that agent's levers).
func TestUnit_ExplainTurnStop_AttachesMetaAndLeavesExternalAlone(t *testing.T) {
	tr := &Transport{}
	native := &sessionEntry{driver: &nativeDriver{t: tr}}

	resp := tr.explainTurnStop(context.Background(), "sess-1", native,
		libacp.PromptResponse{StopReason: libacp.StopReasonMaxTokens})
	require.NotEmpty(t, resp.Meta, "a short ending must carry its explanation")
	var envelope map[string]stopReasonExplained
	require.NoError(t, json.Unmarshal(resp.Meta, &envelope))
	require.Equal(t, string(libacp.StopReasonMaxTokens), envelope[stopReasonMetaKey].Reason)
	require.Contains(t, envelope[stopReasonMetaKey].Command, "/max-tokens")

	endTurn := tr.explainTurnStop(context.Background(), "sess-1", native,
		libacp.PromptResponse{StopReason: libacp.StopReasonEndTurn})
	require.Empty(t, endTurn.Meta)

	external := &sessionEntry{driver: &externalDriver{t: tr, agentName: "downstream"}}
	relayed := tr.explainTurnStop(context.Background(), "sess-1", external,
		libacp.PromptResponse{StopReason: libacp.StopReasonMaxTokens})
	require.Empty(t, relayed.Meta, "an external turn's stop reason belongs to its downstream agent")
}

// TestLoopback_MaxTokensStop_ReachesTheClientAsASentence drives a truncated
// turn through a real ACP wire and pins what an editor actually receives: the
// explanation as an agent message it already knows how to render, and the same
// trio on the prompt response for a client that renders its own chrome.
func TestLoopback_MaxTokensStop_ReachesTheClientAsASentence(t *testing.T) {
	h := newLoopbackHarness(t)
	ctx := context.Background()

	_, err := h.client.Initialize(ctx, libacp.InitializeRequest{ProtocolVersion: libacp.ProtocolVersion})
	require.NoError(t, err)
	newResp, err := h.client.NewSession(ctx, libacp.NewSessionRequest{Cwd: t.TempDir(), McpServers: []libacp.McpServer{}})
	require.NoError(t, err)
	h.lc.drain(t, 1)

	h.swapAgent(newResp.SessionID, &loopbackAgent{
		promptFunc: func(context.Context, agentservice.PromptRequest) (*agentservice.PromptResponse, error) {
			return &agentservice.PromptResponse{StopReason: agentservice.StopMaxTokens}, nil
		},
	})

	resp, err := h.client.Prompt(ctx, libacp.PromptRequest{
		SessionID: newResp.SessionID,
		Prompt:    []libacp.ContentBlock{libacp.NewTextContent("write me something long")},
	})
	require.NoError(t, err)
	require.Equal(t, libacp.StopReasonMaxTokens, resp.StopReason)

	var envelope map[string]stopReasonExplained
	require.NoError(t, json.Unmarshal(resp.Meta, &envelope))
	require.Equal(t, string(libacp.StopReasonMaxTokens), envelope[stopReasonMetaKey].Reason)

	text := waitForAgentMessage(t, h, newResp.SessionID, "/max-tokens")
	require.Contains(t, text, "token cap")
}

// waitForAgentMessage reads the update stream until an agent message for sid
// contains want, failing the test if it never arrives.
func waitForAgentMessage(t *testing.T, h *loopbackHarness, sid libacp.SessionID, want string) string {
	t.Helper()
	deadline := time.After(30 * time.Second)
	for {
		select {
		case note := <-h.lc.updates:
			if note.SessionID != sid || note.Update.SessionUpdate != libacp.SessionUpdateAgentMessageChunk {
				continue
			}
			if strings.Contains(note.Update.Content.Text, want) {
				return note.Update.Content.Text
			}
		case <-deadline:
			t.Fatalf("no agent message containing %q arrived for session %s", want, sid)
		}
	}
}

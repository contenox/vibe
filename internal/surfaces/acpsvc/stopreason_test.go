package acpsvc

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/contenox/contenox/internal/kernel/nativeturn"
	"github.com/contenox/contenox/internal/services/agentservice"
	libacp "github.com/contenox/contenox/libacp"
	"github.com/stretchr/testify/require"
)

// TestUnit_ExplainStopReason_EveryShortEndingHasASentence pins that no non-
// end_turn stop reason travels as a bare token: each carries what happened and,
// where one exists, the command that resolves it.
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

// TestUnit_ExplainSuspension_IsNotAnACPStopReasonAndNamesTheApproval pins the
// protocol decision behind a parked turn: "suspended" stays out of ACP's
// closed stopReason set and rides `_meta` instead, and the explanation names
// the approval so a client can say what it is waiting on.
func TestUnit_ExplainSuspension_IsNotAnACPStopReasonAndNamesTheApproval(t *testing.T) {
	for _, r := range []libacp.StopReason{
		libacp.StopReasonEndTurn,
		libacp.StopReasonMaxTokens,
		libacp.StopReasonMaxTurnRequests,
		libacp.StopReasonRefusal,
		libacp.StopReasonCancelled,
	} {
		require.NotEqual(t, stopReasonSuspended, string(r),
			"a suspension must not be smuggled in as an ACP stop reason")
	}
	require.Equal(t, libacp.StopReasonEndTurn, mapStopReason(agentservice.StopSuspended),
		"the spec field stays inside ACP's set; _meta carries the truth")

	explained := explainSuspension("ead905ab-d548")
	require.Equal(t, stopReasonSuspended, explained.Reason)
	require.Equal(t, "ead905ab-d548", explained.ApprovalID)
	require.Contains(t, explained.Command, "ead905ab-d548")
	require.Contains(t, explained.Explanation, "suspended")

	var envelope map[string]stopReasonExplained
	require.NoError(t, json.Unmarshal(suspensionMeta("ead905ab-d548"), &envelope))
	require.Equal(t, stopReasonSuspended, envelope[stopReasonMetaKey].Reason)
	require.Equal(t, "ead905ab-d548", envelope[stopReasonMetaKey].ApprovalID)

	notice := suspensionNotice("ead905ab-d548")
	require.Equal(t, libacp.SessionUpdateAgentMessageChunk, notice.SessionUpdate)
	require.Contains(t, notice.Content.Text, "ead905ab-d548",
		"the id must be readable by a client that renders only the message")
	require.NotEmpty(t, notice.Meta, "and machine-readable by one that does not")
}

// TestUnit_NativeResultToResponse_SuspendedCarriesItsMeta pins the survival
// path: a client that reattaches resolves its prompt from the turn Result, so
// the suspension must be attached there too, not only on the live path.
func TestUnit_NativeResultToResponse_SuspendedCarriesItsMeta(t *testing.T) {
	parked, err := nativeResultToResponse(nativeturn.Result{
		StopReason: libacp.StopReasonEndTurn,
		Suspended:  true,
		ApprovalID: "ead905ab-d548",
	})
	require.NoError(t, err)
	require.NotEmpty(t, parked.Meta)
	var envelope map[string]stopReasonExplained
	require.NoError(t, json.Unmarshal(parked.Meta, &envelope))
	require.Equal(t, "ead905ab-d548", envelope[stopReasonMetaKey].ApprovalID)

	finished, err := nativeResultToResponse(nativeturn.Result{StopReason: libacp.StopReasonEndTurn})
	require.NoError(t, err)
	require.Empty(t, finished.Meta, "a completed turn stays bare — that is what makes the park distinguishable")
}

// TestLoopback_ParkedTurn_ReportsSuspendedAndNamesTheApproval is the regression
// for the incident: a turn that parks on a human approval reached every client
// as stop_reason end_turn and nothing else, so a working suspension read as a
// dead agent.
func TestLoopback_ParkedTurn_ReportsSuspendedAndNamesTheApproval(t *testing.T) {
	h := newLoopbackHarness(t)
	ctx := context.Background()

	_, err := h.client.Initialize(ctx, libacp.InitializeRequest{ProtocolVersion: libacp.ProtocolVersion})
	require.NoError(t, err)
	newResp, err := h.client.NewSession(ctx, libacp.NewSessionRequest{Cwd: t.TempDir(), McpServers: []libacp.McpServer{}})
	require.NoError(t, err)
	h.lc.drain(t, 1)

	const approvalID = "ead905ab-0000-0000-0000-00000000d548"
	h.swapAgent(newResp.SessionID, &loopbackAgent{
		promptFunc: func(context.Context, agentservice.PromptRequest) (*agentservice.PromptResponse, error) {
			return &agentservice.PromptResponse{
				StopReason:          agentservice.StopSuspended,
				SuspendedApprovalID: approvalID,
			}, nil
		},
	})

	resp, err := h.client.Prompt(ctx, libacp.PromptRequest{
		SessionID: newResp.SessionID,
		Prompt:    []libacp.ContentBlock{libacp.NewTextContent("find the project")},
	})
	require.NoError(t, err)

	var envelope map[string]stopReasonExplained
	require.NotEmpty(t, resp.Meta, "a parked turn must not answer like a finished one")
	require.NoError(t, json.Unmarshal(resp.Meta, &envelope))
	require.Equal(t, stopReasonSuspended, envelope[stopReasonMetaKey].Reason)
	require.Equal(t, approvalID, envelope[stopReasonMetaKey].ApprovalID,
		"the client must be able to say which approval it is waiting on")

	text := waitForAgentMessage(t, h, newResp.SessionID, approvalID)
	require.Contains(t, text, "suspended")

	h.swapAgent(newResp.SessionID, &loopbackAgent{
		promptFunc: func(context.Context, agentservice.PromptRequest) (*agentservice.PromptResponse, error) {
			return &agentservice.PromptResponse{StopReason: agentservice.StopEndTurn}, nil
		},
	})
	done, err := h.client.Prompt(ctx, libacp.PromptRequest{
		SessionID: newResp.SessionID,
		Prompt:    []libacp.ContentBlock{libacp.NewTextContent("done")},
	})
	require.NoError(t, err)
	require.Equal(t, libacp.StopReasonEndTurn, done.StopReason)
	require.Empty(t, done.Meta, "a completed turn carries no suspension explanation")
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

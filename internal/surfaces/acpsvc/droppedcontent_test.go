package acpsvc

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"testing"

	"github.com/contenox/contenox/internal/kernel/nativeturn"
	"github.com/contenox/contenox/internal/services/agentservice"
	libacp "github.com/contenox/contenox/libacp"
	"github.com/stretchr/testify/require"
)

// TestUnit_ExplainDroppedContent_AbsenceIsTheSignal pins the rule that makes
// the envelope readable: a turn that dropped nothing emits no envelope, no
// notice, and no `_meta` at all, so a client can treat presence alone as "the
// prompt lost something" without decoding the payload.
func TestUnit_ExplainDroppedContent_AbsenceIsTheSignal(t *testing.T) {
	_, ok := explainDroppedContent(nil, "")
	require.False(t, ok, "an intact prompt has nothing to explain")
	_, ok = explainDroppedContent([]string{}, "")
	require.False(t, ok)

	_, announced := droppedContentNotice(nil, "")
	require.False(t, announced, "an intact prompt announces nothing")

	bare := withDroppedContentMeta(libacp.PromptResponse{StopReason: libacp.StopReasonEndTurn}, nil)
	require.Empty(t, bare.Meta, "an ordinary turn must stay bare on the wire")

	report, ok := explainDroppedContent([]string{string(libacp.ContentKindImage)}, "")
	require.True(t, ok)
	require.Equal(t, []string{string(libacp.ContentKindImage)}, report.Kinds)
	require.Contains(t, report.Explanation, string(libacp.ContentKindImage))

	notice, announced := droppedContentNotice([]string{string(libacp.ContentKindImage)}, "")
	require.True(t, announced)
	require.Equal(t, libacp.SessionUpdateAgentMessageChunk, notice.SessionUpdate)
	require.Contains(t, notice.Content.Text, string(libacp.ContentKindImage),
		"a client that renders only the message must still read what was lost")
	require.NotEmpty(t, notice.Meta, "and one that renders nothing custom must not be the only one served")
}

// TestUnit_MergeMeta_SiblingEnvelopesDoNotDisplaceEachOther pins the coexistence
// rule: contenox.droppedContent and contenox.stopReason share one `_meta`
// object, so writing either must leave the other intact — a turn can both park
// on an approval and have discarded an attachment.
func TestUnit_MergeMeta_SiblingEnvelopesDoNotDisplaceEachOther(t *testing.T) {
	parked := libacp.PromptResponse{
		StopReason: libacp.StopReasonEndTurn,
		Meta:       suspensionMeta("ead905ab-d548"),
	}
	both := withDroppedContentMeta(parked, []string{string(libacp.ContentKindImage)})

	var envelope struct {
		StopReason stopReasonExplained  `json:"contenox.stopReason"`
		Dropped    droppedContentReport `json:"contenox.droppedContent"`
	}
	require.NoError(t, json.Unmarshal(both.Meta, &envelope))
	require.Equal(t, stopReasonSuspended, envelope.StopReason.Reason)
	require.Equal(t, "ead905ab-d548", envelope.StopReason.ApprovalID)
	require.Equal(t, []string{string(libacp.ContentKindImage)}, envelope.Dropped.Kinds)

	// And in the other order: the transport-boundary explanation is merged onto
	// a response the driver already marked lossy, never assigned over it.
	tr := &Transport{}
	native := &sessionEntry{driver: &nativeDriver{t: tr}}
	lossy := withDroppedContentMeta(
		libacp.PromptResponse{StopReason: libacp.StopReasonMaxTokens},
		[]string{string(libacp.ContentKindImage)})
	explained := tr.explainTurnStop(context.Background(), "sess-1", native, lossy)

	require.NoError(t, json.Unmarshal(explained.Meta, &envelope))
	require.Equal(t, string(libacp.StopReasonMaxTokens), envelope.StopReason.Reason)
	require.Equal(t, []string{string(libacp.ContentKindImage)}, envelope.Dropped.Kinds,
		"explaining the stop reason must not erase what the prompt lost")
}

// TestUnit_NativeResultToResponse_DroppedContentSurvivesReattach pins the
// survival path: a client that reconnects resolves its prompt from the turn
// Result, so reconnecting must not be what makes a discarded attachment
// invisible. A turn that dropped nothing still resolves bare.
func TestUnit_NativeResultToResponse_DroppedContentSurvivesReattach(t *testing.T) {
	lossy, err := nativeResultToResponse(nativeturn.Result{
		StopReason:          libacp.StopReasonEndTurn,
		DroppedContentKinds: []string{string(libacp.ContentKindImage)},
	})
	require.NoError(t, err)
	var envelope map[string]droppedContentReport
	require.NoError(t, json.Unmarshal(lossy.Meta, &envelope))
	require.Equal(t, []string{string(libacp.ContentKindImage)}, envelope[droppedContentMetaKey].Kinds)

	parked, err := nativeResultToResponse(nativeturn.Result{
		StopReason:          libacp.StopReasonEndTurn,
		Suspended:           true,
		ApprovalID:          "ead905ab-d548",
		DroppedContentKinds: []string{string(libacp.ContentKindImage)},
	})
	require.NoError(t, err)
	var both map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(parked.Meta, &both))
	require.Contains(t, both, stopReasonMetaKey)
	require.Contains(t, both, droppedContentMetaKey)

	intact, err := nativeResultToResponse(nativeturn.Result{StopReason: libacp.StopReasonEndTurn})
	require.NoError(t, err)
	require.Empty(t, intact.Meta, "a turn that forwarded everything carries no marker")
}

// TestLoopback_DroppedImage_ReachesTheClientOnTheWire is the regression for the
// incident: a prompt whose image could not be forwarded produced a normal,
// successful turn with the loss recorded only in the tracker, so a client with
// a camera button got an answer written as if no photo had been sent. It drives
// a lossy prompt through a real ACP wire and pins both halves of the report —
// the envelope on the response and the agent message every editor renders —
// against an intact prompt on the same session, which carries neither.
func TestLoopback_DroppedImage_ReachesTheClientOnTheWire(t *testing.T) {
	h := newLoopbackHarness(t)
	ctx := context.Background()

	_, err := h.client.Initialize(ctx, libacp.InitializeRequest{ProtocolVersion: libacp.ProtocolVersion})
	require.NoError(t, err)
	newResp, err := h.client.NewSession(ctx, libacp.NewSessionRequest{Cwd: t.TempDir(), McpServers: []libacp.McpServer{}})
	require.NoError(t, err)
	h.lc.drain(t, 1)

	h.swapAgent(newResp.SessionID, &loopbackAgent{
		promptFunc: func(context.Context, agentservice.PromptRequest) (*agentservice.PromptResponse, error) {
			return &agentservice.PromptResponse{StopReason: agentservice.StopEndTurn}, nil
		},
	})

	// An undecodable image is the reachable shape of "the path could not carry
	// it": extractImageParts leaves it in the text blocks, where FlattenContent
	// reports it dropped.
	resp, err := h.client.Prompt(ctx, libacp.PromptRequest{
		SessionID: newResp.SessionID,
		Prompt: []libacp.ContentBlock{
			libacp.NewTextContent("what is in this photo?"),
			libacp.NewImageContent("not-base64!!", "image/png"),
		},
	})
	require.NoError(t, err)
	require.Equal(t, libacp.StopReasonEndTurn, resp.StopReason)

	require.NotEmpty(t, resp.Meta, "a lossy turn must not answer like an intact one")
	var envelope map[string]droppedContentReport
	require.NoError(t, json.Unmarshal(resp.Meta, &envelope))
	require.Equal(t, []string{string(libacp.ContentKindImage)}, envelope[droppedContentMetaKey].Kinds)

	waitForAgentMessage(t, h, newResp.SessionID, string(libacp.ContentKindImage))

	intact, err := h.client.Prompt(ctx, libacp.PromptRequest{
		SessionID: newResp.SessionID,
		Prompt:    []libacp.ContentBlock{libacp.NewTextContent("and now just text")},
	})
	require.NoError(t, err)
	require.Empty(t, intact.Meta, "a prompt that lost nothing carries no dropped-content marker")
}

// TestLoopback_SlashCommandDropsItsAttachment pins the command path, which
// discards an image that decoded perfectly well — a slash command is a text
// verb and has no use for it. The turn still succeeds; what changes is that the
// client is now told.
func TestLoopback_SlashCommandDropsItsAttachment(t *testing.T) {
	h := newLoopbackHarness(t)
	ctx := context.Background()

	_, err := h.client.Initialize(ctx, libacp.InitializeRequest{ProtocolVersion: libacp.ProtocolVersion})
	require.NoError(t, err)
	newResp, err := h.client.NewSession(ctx, libacp.NewSessionRequest{Cwd: t.TempDir(), McpServers: []libacp.McpServer{}})
	require.NoError(t, err)
	h.lc.drain(t, 1)

	png := base64.StdEncoding.EncodeToString([]byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a})
	resp, err := h.client.Prompt(ctx, libacp.PromptRequest{
		SessionID: newResp.SessionID,
		Prompt: []libacp.ContentBlock{
			libacp.NewTextContent("/help"),
			libacp.NewImageContent(png, "image/png"),
		},
	})
	require.NoError(t, err)

	var envelope map[string]droppedContentReport
	require.NotEmpty(t, resp.Meta, "a command that threw the attachment away must say so")
	require.NoError(t, json.Unmarshal(resp.Meta, &envelope))
	require.Equal(t, []string{string(libacp.ContentKindImage)}, envelope[droppedContentMetaKey].Kinds)
}

// TestLoopback_ParkedTurnWithDroppedImage_ReportsBothFacts pins that the two
// `_meta` envelopes coexist end to end: a turn that both parks on an approval
// and discards an attachment must let a client read either fact, since neither
// implies the other and either alone is a misleading answer.
func TestLoopback_ParkedTurnWithDroppedImage_ReportsBothFacts(t *testing.T) {
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
		Prompt: []libacp.ContentBlock{
			libacp.NewTextContent("read this and act on it"),
			libacp.NewImageContent("not-base64!!", "image/png"),
		},
	})
	require.NoError(t, err)

	var envelope struct {
		StopReason stopReasonExplained  `json:"contenox.stopReason"`
		Dropped    droppedContentReport `json:"contenox.droppedContent"`
	}
	require.NoError(t, json.Unmarshal(resp.Meta, &envelope))
	require.Equal(t, stopReasonSuspended, envelope.StopReason.Reason)
	require.Equal(t, approvalID, envelope.StopReason.ApprovalID)
	require.Equal(t, []string{string(libacp.ContentKindImage)}, envelope.Dropped.Kinds,
		"a park must not be the reason a discarded attachment goes unreported")
}

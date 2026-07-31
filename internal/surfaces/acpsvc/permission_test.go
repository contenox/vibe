package acpsvc

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/contenox/contenox/internal/services/agentservice"
	"github.com/contenox/contenox/internal/services/approvalflow"
	"github.com/contenox/contenox/internal/services/hitlservice"
	"github.com/contenox/contenox/internal/store/runtimetypes"
	"github.com/contenox/libacp"
	"github.com/stretchr/testify/require"
)

// TestUnit_HitlPolicyPath pins how acpsvc resolves a policy name to its
// on-disk path: "<ContenoxDir>/<name>", the same layout
// writeEmbeddedHITLPolicies (contenoxcli/hitl_policies.go) writes to and
// acpPolicySource (contenoxcli/acp_cmd.go) reads from. Empty inputs yield ""
// rather than a bogus join, since a setup-only transport has no ContenoxDir.
func TestUnit_HitlPolicyPath(t *testing.T) {
	tr := &Transport{}
	tr.deps.ContenoxDir = filepath.Join(string(filepath.Separator), "home", "op", ".contenox")

	require.Equal(t, filepath.Join(tr.deps.ContenoxDir, "hitl-policy-strict.json"), tr.hitlPolicyPath("hitl-policy-strict.json"))
	require.Equal(t, "", tr.hitlPolicyPath(""), "no policy name yields no path")
	require.Equal(t, "", tr.hitlPolicyPath("  "), "a blank policy name yields no path")

	tr.deps.ContenoxDir = ""
	require.Equal(t, "", tr.hitlPolicyPath("hitl-policy-strict.json"), "no ContenoxDir yields no path")
}

// TestUnit_AskApproval_ForwardsPolicyNameAndPath drives AskApproval through a
// real ACP wire (loopback harness) and asserts the outbound
// session/request_permission Meta carries the policy name/path and matched
// rule that hitlservice.EvaluationResult attached to the ApprovalRequest —
// the defect this closes: the card previously said only "requires approval",
// never why.
func TestUnit_AskApproval_ForwardsPolicyNameAndPath(t *testing.T) {
	h := newLoopbackHarness(t)
	h.tr.deps.ContenoxDir = "/home/op/.contenox"
	ctx := context.Background()

	_, err := h.client.Initialize(ctx, libacp.InitializeRequest{ProtocolVersion: libacp.ProtocolVersion})
	require.NoError(t, err)
	newResp, err := h.client.NewSession(ctx, libacp.NewSessionRequest{Cwd: t.TempDir(), McpServers: []libacp.McpServer{}})
	require.NoError(t, err)
	h.lc.drain(t, 1)

	rule := 3
	var approvalErr error
	var allowed bool
	fake := &loopbackAgent{promptFunc: func(ctx context.Context, req agentservice.PromptRequest) (*agentservice.PromptResponse, error) {
		approveCtx := context.WithValue(ctx, runtimetypes.SessionIDContextKey, req.SessionID)
		allowed, approvalErr = h.tr.AskApproval(approveCtx, hitlservice.ApprovalRequest{
			ToolCallID:  "call-perm-meta",
			ToolsName:   "local_fs",
			ToolName:    "write_file",
			Args:        map[string]any{"path": "/tmp/x"},
			PolicyName:  "hitl-policy-strict.json",
			MatchedRule: &rule,
		})
		return &agentservice.PromptResponse{StopReason: agentservice.StopEndTurn}, nil
	}}
	h.swapAgent(newResp.SessionID, fake)

	h.lc.setPermissionResponse(libacp.RequestPermissionResponse{
		Outcome: libacp.RequestPermissionOutcome{Outcome: libacp.PermissionOutcomeSelected, OptionID: approvalflow.OptionAllow},
	})
	_, err = h.client.Prompt(ctx, libacp.PromptRequest{
		SessionID: newResp.SessionID,
		Prompt:    []libacp.ContentBlock{libacp.NewTextContent("write the file")},
	})
	require.NoError(t, err)
	require.NoError(t, approvalErr)
	require.True(t, allowed)

	req, ok := h.lc.lastPermissionRequest()
	require.True(t, ok, "the real client must have received session/request_permission")

	meta, ok := approvalflow.ParseMeta(req.Meta)
	require.True(t, ok, "request must carry a non-empty _meta envelope")
	require.Equal(t, "hitl-policy-strict.json", meta.PolicyName, "policy name must reach the wire")
	require.Equal(t, filepath.Join("/home/op/.contenox", "hitl-policy-strict.json"), meta.PolicyPath, "policy path must reach the wire")
	require.NotNil(t, meta.MatchedRule)
	require.Equal(t, 3, *meta.MatchedRule, "matched rule index must reach the wire")

	toolCallMeta, ok := approvalflow.ParseMeta(req.ToolCall.Meta)
	require.True(t, ok, "toolCall.Meta carries the same envelope")
	require.Equal(t, "hitl-policy-strict.json", toolCallMeta.PolicyName)
}

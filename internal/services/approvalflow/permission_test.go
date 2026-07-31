package approvalflow

import (
	"encoding/json"
	"testing"

	"github.com/contenox/contenox/internal/services/hitlservice"
	"github.com/contenox/libacp"
	"github.com/stretchr/testify/require"
)

func TestUnit_BuildRequest_CarriesSedInputAndDiffForApprovalCard(t *testing.T) {
	oldText := "Questions: hello@contenox.com\n"
	newText := "Questions: **hello@contenox.com**\n"
	unified := "--- README.md\n+++ README.md\n@@\n-Questions: hello@contenox.com\n+Questions: **hello@contenox.com**\n"

	req := BuildRequest(hitlservice.ApprovalRequest{
		ToolCallID: "call-1",
		ToolsName:  "local_fs",
		ToolName:   "sed",
		Args: map[string]any{
			"path":        "README.md",
			"pattern":     "Questions: hello@contenox.com",
			"replacement": "Questions: **hello@contenox.com**",
		},
		Diff:    unified,
		DiffOld: oldText,
		DiffNew: newText,
	}, BuildOptions{
		SessionID:  "session-1",
		PolicyName: "hitl-policy-strict.json",
		PolicyPath: "/home/user/.contenox/hitl-policy-strict.json",
	})

	require.Equal(t, libacp.SessionID("session-1"), req.SessionID)
	require.Equal(t, "call-1", req.ToolCall.ToolCallID)
	require.Equal(t, "local_fs.sed: Questions: hello@contenox.com in README.md", req.ToolCall.Title)
	require.Equal(t, libacp.ToolKindEdit, req.ToolCall.Kind)
	require.JSONEq(t, `{"path":"README.md","pattern":"Questions: hello@contenox.com","replacement":"Questions: **hello@contenox.com**"}`, string(req.ToolCall.RawInput))

	require.Len(t, req.ToolCall.Content, 1)
	require.Equal(t, libacp.ToolCallContentDiff, req.ToolCall.Content[0].Type)
	require.Equal(t, "README.md", req.ToolCall.Content[0].Path)
	require.Equal(t, oldText, req.ToolCall.Content[0].OldText)
	require.Equal(t, newText, req.ToolCall.Content[0].NewText)

	var meta Meta
	require.NoError(t, json.Unmarshal(req.ToolCall.Meta, &meta))
	require.Equal(t, "local_fs", meta.ToolsName)
	require.Equal(t, "sed", meta.ToolName)
	require.Equal(t, "hitl-policy-strict.json", meta.PolicyName)
	require.Equal(t, "/home/user/.contenox/hitl-policy-strict.json", meta.PolicyPath)
	require.Equal(t, unified, meta.Diff)
	require.Equal(t, oldText, meta.DiffOld)
	require.Equal(t, newText, meta.DiffNew)
}

func TestUnit_DiffContent_BlankOldAndNewYieldsNil(t *testing.T) {
	require.Nil(t, DiffContent("README.md", "", ""))
}

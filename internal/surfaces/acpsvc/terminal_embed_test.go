package acpsvc

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/contenox/beam/internal/kernel/taskengine"
	"github.com/contenox/beam/libacp"
	"github.com/stretchr/testify/require"
)

func TestUnit_TerminalAttachNotification_Shape(t *testing.T) {
	note := terminalAttachNotification(libacp.SessionID("sess-1"), "call-7", "term-42", "local_shell: ls -la")
	upd := note.Update

	require.Equal(t, libacp.SessionID("sess-1"), note.SessionID)
	require.Equal(t, libacp.SessionUpdateToolCall, upd.SessionUpdate,
		"must be the full tool_call kind (create-or-update by id) so it is race-safe vs the pending notification")
	require.Equal(t, "call-7", upd.ToolCallID)
	require.Equal(t, "local_shell: ls -la", upd.Title,
		"title is required by the ACP schema on tool_call notifications; "+
			"omitting it causes Zed to reject the update and the terminal embed is lost")
	require.Equal(t, libacp.ToolKindExecute, upd.Kind)
	require.Equal(t, libacp.ToolCallStatusInProgress, upd.Status)
	require.Len(t, upd.ToolContent, 1)
	require.Equal(t, libacp.ToolCallContentTerminal, upd.ToolContent[0].Type)
	require.Equal(t, "term-42", upd.ToolContent[0].TerminalID)

	wire, err := json.Marshal(upd)
	require.NoError(t, err)
	var generic map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(wire, &generic))
	require.Contains(t, generic, "content", "embedded terminal must serialize under the `content` key per ACP")

	var arr []libacp.ToolCallContent
	require.NoError(t, json.Unmarshal(generic["content"], &arr))
	require.Len(t, arr, 1)
	require.Equal(t, libacp.ToolCallContentTerminal, arr[0].Type)
	require.Equal(t, "term-42", arr[0].TerminalID)

	var back libacp.SessionUpdate
	require.NoError(t, json.Unmarshal(wire, &back))
	require.Equal(t, "term-42", back.ToolContent[0].TerminalID, "must round-trip so the client can attach to the live terminal")
}

func TestUnit_ToolCallIDFromCtx(t *testing.T) {
	require.Equal(t, "", toolCallIDFromCtx(context.Background()),
		"no tool-call id in ctx → empty, so acpCommandRunner skips the embed (graceful)")

	ctx := context.WithValue(context.Background(), taskengine.ContextKeyToolCallID, "call-9")
	require.Equal(t, "call-9", toolCallIDFromCtx(ctx),
		"must read the exact taskengine.ContextKeyToolCallID the tool-dispatch sites set; "+
			"if taskexec stops threading it this returns \"\" and live terminal embedding silently disappears")
}

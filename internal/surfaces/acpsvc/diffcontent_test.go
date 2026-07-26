package acpsvc

import (
	"strings"
	"testing"

	"github.com/contenox/beam/internal/kernel/taskengine"
	"github.com/contenox/beam/internal/services/approvalflow"
	libacp "github.com/contenox/beam/libacp"
	"github.com/stretchr/testify/require"
)

func TestUnit_DiffContent_CarriesRawOldNewNotRenderedString(t *testing.T) {
	oldText := "line one\nline two\n"
	newText := "line one\nline two\nline three\n"

	content := approvalflow.DiffContent("README.md", oldText, newText)

	require.Len(t, content, 1)
	require.Equal(t, libacp.ToolCallContentDiff, content[0].Type)
	require.Equal(t, "README.md", content[0].Path)
	require.Equal(t, oldText, content[0].OldText)
	require.Equal(t, newText, content[0].NewText)
	require.NotContains(t, content[0].NewText, "@@")
	require.NotContains(t, content[0].NewText, "--- README.md")
}

func TestUnit_DiffContent_NoChangeOrNoPathYieldsNil(t *testing.T) {
	require.Nil(t, approvalflow.DiffContent("README.md", "same\n", "same\n"))
	require.Nil(t, approvalflow.DiffContent("", "old", "new"))
	require.Nil(t, approvalflow.DiffContent("README.md", "", ""))
}

func TestUnit_ToolCallUpdate_DeniedShellRetainsCommand(t *testing.T) {
	ev := taskengine.TaskEvent{
		Kind:     taskengine.TaskEventToolCall,
		ToolName: "local_shell",
		Content:  "User denied the operation. Please ask for clarification or try a different, less destructive approach.",
		ApprovalArgs: map[string]any{
			"command": "sh",
			"args":    "-c \"git diff README.md\"",
		},
	}

	note := toolCallUpdateNotification(libacp.SessionID("sess-1"), ev, fallbackToolCallID(ev))

	require.Len(t, note.Update.ToolContent, 0)
	require.Contains(t, note.Update.Title, "sh")
	require.True(t, strings.Contains(note.Update.Title, "git diff"))
}

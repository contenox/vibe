package acpsvc

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/contenox/beam/internal/kernel/taskengine"
	"github.com/contenox/beam/internal/services/localtools"
	"github.com/contenox/beam/libacp"
	"github.com/stretchr/testify/require"
)

func TestUnit_ToolCallUpdate_FsWriteResultProducesDiff(t *testing.T) {
	fw := localtools.FsWriteResult{
		Path:    "/tmp/abc.txt",
		OldText: "old",
		NewText: "new",
		Written: true,
	}
	raw, err := json.Marshal(fw)
	require.NoError(t, err)
	require.NotContains(t, string(raw), "old_text", "model-visible write_file JSON must stay compact")
	require.NotContains(t, string(raw), "new_text", "model-visible write_file JSON must stay compact")

	ev := taskengine.TaskEvent{
		Kind:            taskengine.TaskEventToolCall,
		ToolName:        "local_fs.write_file",
		ApprovalID:      "call-1",
		ApprovalArgs:    map[string]any{"path": "/tmp/abc.txt", "content": "new"},
		Content:         string(raw),
		ToolDiffPath:    fw.Path,
		ToolDiffOldText: fw.OldText,
		ToolDiffNewText: fw.NewText,
	}

	note := toolCallUpdateNotification(libacp.SessionID("sess-1"), ev, fallbackToolCallID(ev))
	upd := note.Update

	require.Equal(t, libacp.SessionUpdateToolCallUpdate, upd.SessionUpdate)
	require.Equal(t, "call-1", upd.ToolCallID)
	require.Equal(t, "local_fs.write_file: /tmp/abc.txt", upd.Title)
	require.Equal(t, libacp.ToolKindEdit, upd.Kind)
	require.Equal(t, libacp.ToolCallStatusCompleted, upd.Status)
	require.Len(t, upd.ToolContent, 1)
	require.Equal(t, libacp.ToolCallContentDiff, upd.ToolContent[0].Type)
	require.Equal(t, "/tmp/abc.txt", upd.ToolContent[0].Path)
	require.Equal(t, "old", upd.ToolContent[0].OldText)
	require.Equal(t, "new", upd.ToolContent[0].NewText)

	wire, err := json.Marshal(upd)
	require.NoError(t, err)

	var generic map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(wire, &generic))
	require.Contains(t, generic, "content", "tool_call update must serialize ToolContent under the `content` key per ACP spec")
	require.NotContains(t, generic, "content_list")

	var contentArr []libacp.ToolCallContent
	require.NoError(t, json.Unmarshal(generic["content"], &contentArr))
	require.Len(t, contentArr, 1)
	require.Equal(t, libacp.ToolCallContentDiff, contentArr[0].Type)
}

func TestUnit_ToolCallUpdate_NonFsResultHasNoDiff(t *testing.T) {
	ev := taskengine.TaskEvent{
		Kind:     taskengine.TaskEventToolCall,
		ToolName: "echo",
		Content:  "\"hello\"",
	}
	note := toolCallUpdateNotification(libacp.SessionID("sess-1"), ev, fallbackToolCallID(ev))
	require.Len(t, note.Update.ToolContent, 0)
	require.Equal(t, libacp.ToolKindOther, note.Update.Kind)
}

func TestUnit_ToolCallUpdate_ErrorMarksFailed(t *testing.T) {
	ev := taskengine.TaskEvent{
		Kind:     taskengine.TaskEventToolCall,
		ToolName: "local_shell.exec",
		Error:    "boom",
		Content:  "stderr: boom",
	}
	note := toolCallUpdateNotification(libacp.SessionID("sess-1"), ev, fallbackToolCallID(ev))
	require.Equal(t, libacp.ToolCallStatusFailed, note.Update.Status)
	require.Equal(t, libacp.ToolKindExecute, note.Update.Kind)
}

func TestUnit_NormalizeToolCallNotification_PromotesUnknownUpdate(t *testing.T) {
	tr := &Transport{}
	note := libacp.SessionNotification{
		SessionID: "sess-1",
		Update: libacp.SessionUpdate{
			SessionUpdate: libacp.SessionUpdateToolCallUpdate,
			ToolCallID:    "call-1",
			Title:         "local_fs.read_file",
			Kind:          libacp.ToolKindRead,
			Status:        libacp.ToolCallStatusFailed,
			Meta:          json.RawMessage(`{"error":"not allowed"}`),
		},
	}

	got := tr.normalizeToolCallNotification(note)

	require.Equal(t, libacp.SessionUpdateToolCall, got.Update.SessionUpdate,
		"Zed reports 'Tool call not found' when the first notification for an id is tool_call_update")
	require.Equal(t, "call-1", got.Update.ToolCallID)
	require.Equal(t, "local_fs.read_file", got.Update.Title)
	require.Equal(t, libacp.ToolKindRead, got.Update.Kind)
	require.Equal(t, libacp.ToolCallStatusFailed, got.Update.Status)
	require.JSONEq(t, `{"error":"not allowed"}`, string(got.Update.Meta))

	next := tr.normalizeToolCallNotification(libacp.SessionNotification{
		SessionID: "sess-1",
		Update: libacp.SessionUpdate{
			SessionUpdate: libacp.SessionUpdateToolCallUpdate,
			ToolCallID:    "call-1",
			Status:        libacp.ToolCallStatusFailed,
		},
	})
	require.Equal(t, libacp.SessionUpdateToolCallUpdate, next.Update.SessionUpdate,
		"once the id is known, normal updates should stay compact")
}

func TestUnit_NormalizeToolCallNotification_UnknownUpdateGetsSchemaFallbacks(t *testing.T) {
	tr := &Transport{}
	got := tr.normalizeToolCallNotification(libacp.SessionNotification{
		SessionID: "sess-1",
		Update: libacp.SessionUpdate{
			SessionUpdate: libacp.SessionUpdateToolCallUpdate,
			ToolCallID:    "orphan-result",
			Status:        libacp.ToolCallStatusCompleted,
			RawOutput:     json.RawMessage(`"ok"`),
		},
	})

	require.Equal(t, libacp.SessionUpdateToolCall, got.Update.SessionUpdate)
	require.Equal(t, "orphan-result", got.Update.Title,
		"promoted tool_call notifications need a title for ACP clients that validate create/update shape")
	require.Equal(t, libacp.ToolKindOther, got.Update.Kind)
	require.JSONEq(t, `"ok"`, string(got.Update.RawOutput))
}

func TestUnit_NormalizeToolCallNotification_DoesNotDowngradeStatus(t *testing.T) {
	tr := &Transport{}
	inProgress := terminalAttachNotification("sess-1", "call-1", "term-1", "local_shell: go test")
	got := tr.normalizeToolCallNotification(inProgress)
	require.Equal(t, libacp.ToolCallStatusInProgress, got.Update.Status)

	pending := tr.normalizeToolCallNotification(libacp.SessionNotification{
		SessionID: "sess-1",
		Update: libacp.SessionUpdate{
			SessionUpdate: libacp.SessionUpdateToolCall,
			ToolCallID:    "call-1",
			Title:         "local_shell.local_shell: go test",
			Kind:          libacp.ToolKindExecute,
			Status:        libacp.ToolCallStatusPending,
			RawInput:      json.RawMessage(`{"command":"go","args":["test"]}`),
		},
	})

	require.Equal(t, libacp.SessionUpdateToolCall, pending.Update.SessionUpdate)
	require.Equal(t, libacp.ToolCallStatusInProgress, pending.Update.Status,
		"terminal embedding can reach the client before the pending event; later pending metadata must not rewind the card")
	require.JSONEq(t, `{"command":"go","args":["test"]}`, string(pending.Update.RawInput))
}

// TestUnit_IsToolBearingHandler pins THIS translator's use of the shared
// predicate. The predicate itself (and the full handler matrix) is owned and
// exhaustively tested in taskengine, which is the point of moving it there: a new
// handler is now covered for both translators at once instead of only this one.
func TestUnit_IsToolBearingHandler(t *testing.T) {
	require.True(t, taskengine.IsToolBearingHandler(string(taskengine.HandleChatCompletion)))
	require.True(t, taskengine.IsToolBearingHandler(string(taskengine.HandleExecuteToolCalls)))
	require.True(t, taskengine.IsToolBearingHandler(string(taskengine.HandleTools)))
	require.True(t, taskengine.IsToolBearingHandler(string(taskengine.HandleRoute)))
	require.False(t, taskengine.IsToolBearingHandler(string(taskengine.HandleNoop)))
}

func TestUnit_ReplayToolCall_FromAssistantMessage(t *testing.T) {
	tc := taskengine.ToolCall{
		ID:   "call-xyz",
		Type: "function",
		Function: taskengine.FunctionCall{
			Name:      "local_fs.read_file",
			Arguments: `{"path":"/tmp/foo.txt"}`,
		},
	}
	upd := toolCallUpdateFromCall(tc)

	require.Equal(t, libacp.SessionUpdateToolCall, upd.SessionUpdate)
	require.Equal(t, "call-xyz", upd.ToolCallID)
	require.Equal(t, "local_fs.read_file: /tmp/foo.txt", upd.Title)
	require.Equal(t, libacp.ToolKindRead, upd.Kind)
	require.Equal(t, libacp.ToolCallStatusCompleted, upd.Status)
	require.JSONEq(t, `{"path":"/tmp/foo.txt"}`, string(upd.RawInput))
}

func TestUnit_ReplayToolCall_InvalidArgumentsOmitsRawInput(t *testing.T) {
	tc := taskengine.ToolCall{
		ID: "call-1",
		Function: taskengine.FunctionCall{
			Name:      "local_shell.exec",
			Arguments: "not-json",
		},
	}
	upd := toolCallUpdateFromCall(tc)
	require.Empty(t, upd.RawInput, "malformed Arguments must not be forwarded as RawInput")
	require.Equal(t, libacp.ToolKindExecute, upd.Kind)
}

func TestUnit_ReplayToolResult_FromToolMessage(t *testing.T) {
	m := taskengine.Message{
		Role:       "tool",
		ToolCallID: "call-xyz",
		Content:    "hello world",
	}
	upd := toolCallUpdateFromResult(m)

	require.Equal(t, libacp.SessionUpdateToolCallUpdate, upd.SessionUpdate)
	require.Equal(t, "call-xyz", upd.ToolCallID)
	require.Equal(t, libacp.ToolCallStatusCompleted, upd.Status)
	require.JSONEq(t, `"hello world"`, string(upd.RawOutput))
	require.Empty(t, upd.ToolContent)
}

func TestUnit_SummarizeToolCallArgs(t *testing.T) {
	cases := []struct {
		name     string
		tool     string
		args     map[string]any
		expected string
	}{
		{"exec without args", "acp_terminal.exec", map[string]any{"command": "ls"}, "ls"},
		{"exec with args slice", "acp_terminal.exec", map[string]any{"command": "git", "args": []any{"status", "--short"}}, "git status --short"},
		{"read_file path", "local_fs.read_file", map[string]any{"path": "/tmp/foo.txt"}, "/tmp/foo.txt"},
		{"local grep pattern+path", "local_fs.grep", map[string]any{"pattern": "TODO", "path": "src/"}, "TODO in src/"},
		{"grep pattern only", "grep", map[string]any{"pattern": "TODO"}, "TODO"},
		{"fetch_url", "webtools.fetch_url", map[string]any{"url": "https://example.com"}, "https://example.com"},
		{"unknown tool returns empty", "foo.bar", map[string]any{"x": "y"}, ""},
		{"missing main arg returns empty", "acp_terminal.exec", map[string]any{}, ""},
		{"newlines collapsed", "acp_terminal.exec", map[string]any{"command": "echo", "args": []any{"a\nb"}}, "echo a b"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			require.Equal(t, c.expected, summarizeToolCallArgs(c.tool, c.args))
		})
	}
}

func TestUnit_SummarizeToolCallArgs_LongCommandTruncates(t *testing.T) {
	long := strings.Repeat("x", 200)
	got := summarizeToolCallArgs("acp_terminal.exec", map[string]any{"command": long})
	require.LessOrEqual(t, len([]rune(got)), 80)
	require.Contains(t, got, "…")
}

func TestUnit_ReplayToolResult_FsWriteProducesDiff(t *testing.T) {
	m := taskengine.Message{
		Role:       "tool",
		ToolCallID: "call-w",
		Content:    `{"path":"/tmp/a.txt","old_text":"old","new_text":"new","written":true}`,
	}
	upd := toolCallUpdateFromResult(m)
	require.Len(t, upd.ToolContent, 1)
	require.Equal(t, libacp.ToolCallContentDiff, upd.ToolContent[0].Type)
	require.Equal(t, "/tmp/a.txt", upd.ToolContent[0].Path)
	require.Equal(t, "old", upd.ToolContent[0].OldText)
	require.Equal(t, "new", upd.ToolContent[0].NewText)
}

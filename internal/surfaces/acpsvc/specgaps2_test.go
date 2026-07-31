package acpsvc

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/contenox/contenox/internal/kernel/taskengine"
	"github.com/contenox/libacp"
	"github.com/stretchr/testify/require"
)

func TestUnit_ToolCallLocations_FromPathArg(t *testing.T) {
	ev := taskengine.TaskEvent{
		Kind:         taskengine.TaskEventToolCall,
		ToolName:     "local_fs.read_file",
		ApprovalID:   "c1",
		ApprovalArgs: map[string]any{"path": "/abs/src/main.go"},
	}
	note := toolCallUpdateNotification(libacp.SessionID("s"), ev, fallbackToolCallID(ev))
	require.Equal(t, []libacp.ToolCallLocation{{Path: "/abs/src/main.go"}}, note.Update.Locations)

	wire, err := json.Marshal(note.Update)
	require.NoError(t, err)
	var generic map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(wire, &generic))
	require.Contains(t, generic, "locations", "tool_call update must serialize file locations under the `locations` key for client follow-along")
}

func TestUnit_ToolCallLocations_PrefersResolvedDiffPath(t *testing.T) {
	ev := taskengine.TaskEvent{
		Kind:            taskengine.TaskEventToolCall,
		ToolName:        "local_fs.write_file",
		ApprovalArgs:    map[string]any{"path": "relative.go"},
		ToolDiffPath:    "/abs/resolved.go",
		ToolDiffOldText: "a",
		ToolDiffNewText: "b",
	}
	note := toolCallUpdateNotification(libacp.SessionID("s"), ev, fallbackToolCallID(ev))
	require.Equal(t, []libacp.ToolCallLocation{{Path: "/abs/resolved.go"}}, note.Update.Locations,
		"the resolved absolute path from the write result must win over the raw model arg")
}

func TestUnit_ToolCallLocations_PendingEmitsLocation(t *testing.T) {
	ev := taskengine.TaskEvent{
		Kind:         taskengine.TaskEventToolCallPending,
		ToolName:     "local_fs.write_file",
		ApprovalArgs: map[string]any{"path": "/abs/x.go", "content": "hi"},
	}
	note := toolCallPendingNotification(libacp.SessionID("s"), ev, fallbackToolCallID(ev))
	require.Equal(t, []libacp.ToolCallLocation{{Path: "/abs/x.go"}}, note.Update.Locations)
}

func TestUnit_ToolCallLocations_NoneForCommandOnlyTool(t *testing.T) {
	ev := taskengine.TaskEvent{
		Kind:         taskengine.TaskEventToolCall,
		ToolName:     "local_shell.exec",
		ApprovalArgs: map[string]any{"command": "ls"},
	}
	note := toolCallUpdateNotification(libacp.SessionID("s"), ev, fallbackToolCallID(ev))
	require.Empty(t, note.Update.Locations)
}

func TestUnit_FlattenPromptBlocks_TextAndResourceLink(t *testing.T) {
	out, dropped := libacp.FlattenContent([]libacp.ContentBlock{
		{Type: string(libacp.ContentKindText), Text: "hello"},
		{Type: string(libacp.ContentKindResourceLink), Name: "spec", URI: "file:///spec.md"},
		{Type: string(libacp.ContentKindResourceLink), URI: "https://x.test"},
	})
	require.Equal(t, "hello\nspec: file:///spec.md\nhttps://x.test", out)
	require.Empty(t, dropped, "resource_link requires no capability and must not be dropped")
}

func TestUnit_FlattenPromptBlocks_DropsUnsupportedAndReportsKinds(t *testing.T) {
	out, dropped := libacp.FlattenContent([]libacp.ContentBlock{
		{Type: string(libacp.ContentKindText), Text: "keep"},
		libacp.NewImageContent("base64", "image/png"),
		libacp.NewImageContent("more", "image/png"),
		{Type: string(libacp.ContentKindResource), Resource: &libacp.EmbeddedResource{URI: "u", Blob: "bytes"}},
	})
	require.Equal(t, "keep", out)
	require.Equal(t, []string{"image", "resource"}, dropped, "dropped kinds must be distinct and ordered, for tracker telemetry")
}

func TestUnit_FlattenPromptBlocks_ResourceTextIncluded(t *testing.T) {
	out, dropped := libacp.FlattenContent([]libacp.ContentBlock{
		libacp.NewResourceContent(libacp.EmbeddedResource{URI: "u", Text: "embedded body"}),
	})
	require.Equal(t, "embedded body", out)
	require.Empty(t, dropped)
}

func TestUnit_NegotiateProtocolVersion(t *testing.T) {
	require.Equal(t, libacp.ProtocolVersion, negotiateProtocolVersion(libacp.ProtocolVersion),
		"supported version must be echoed back")
	require.Equal(t, libacp.ProtocolVersion, negotiateProtocolVersion(0),
		"unsupported (too low) must fall back to the agent's latest")
	require.Equal(t, libacp.ProtocolVersion, negotiateProtocolVersion(libacp.ProtocolVersion+1),
		"unsupported (too high) must fall back to the agent's latest")
	require.Equal(t, libacp.ProtocolVersion, negotiateProtocolVersion(-1))
	require.Equal(t, 1, negotiateProtocolVersion(1), "v1 is in range and must be echoed")
}

func TestUnit_ToolCallInProgressNotification(t *testing.T) {
	ev := taskengine.TaskEvent{
		Kind:       taskengine.TaskEventToolCallPending,
		ToolName:   "local_fs.write_file",
		ApprovalID: "call-3",
	}
	pending := toolCallPendingNotification(libacp.SessionID("s"), ev, fallbackToolCallID(ev))
	inprog := toolCallInProgressNotification(libacp.SessionID("s"), ev)

	require.Equal(t, libacp.SessionUpdateToolCall, pending.Update.SessionUpdate)
	require.Equal(t, libacp.ToolCallStatusPending, pending.Update.Status)

	require.Equal(t, libacp.SessionUpdateToolCallUpdate, inprog.Update.SessionUpdate,
		"in_progress must be a tool_call_update per Prompt-Turn step 5")
	require.Equal(t, libacp.ToolCallStatusInProgress, inprog.Update.Status)
	require.Equal(t, pending.Update.ToolCallID, inprog.Update.ToolCallID,
		"pending and in_progress must share the toolCallId so the client correlates the same card")
	require.Equal(t, libacp.ToolKindEdit, inprog.Update.Kind)
}

func TestUnit_Initialize_EmbeddedContextNotGatedOnClientFS(t *testing.T) {
	tr := &Transport{
		sessions:        make(map[libacp.SessionID]*sessionEntry),
		contenoxToACPID: make(map[string]libacp.SessionID),
	}
	resp, err := tr.Initialize(context.Background(), libacp.InitializeRequest{
		ProtocolVersion:    libacp.ProtocolVersion,
		ClientCapabilities: libacp.ClientCapabilities{FS: libacp.FileSystemCapabilities{ReadTextFile: false}},
	})
	require.NoError(t, err)
	require.True(t, resp.AgentCapabilities.PromptCapabilities.EmbeddedContext,
		"embeddedContext is the agent's ability to consume embedded resources in prompts (libacp.FlattenContent always does); it must NOT be gated on the client's fs.readTextFile, or clients without fs read are wrongly told they cannot send @-mention/file context")
}

func transportWithMeta(meta string) *Transport {
	tr := &Transport{
		sessions:        make(map[libacp.SessionID]*sessionEntry),
		contenoxToACPID: make(map[string]libacp.SessionID),
	}
	if meta != "" {
		tr.clientCaps = libacp.ClientCapabilities{Meta: json.RawMessage(meta)}
	}
	return tr
}

func TestUnit_Authenticate_AcceptsAdvertisedTerminalMethod(t *testing.T) {
	tr := transportWithMeta(`{"terminal-auth":true}`)
	_, err := tr.Authenticate(context.Background(), libacp.AuthenticateRequest{MethodID: terminalAuthMethodID})
	require.NoError(t, err, "the agent must not reject the very auth method it advertised")
}

func TestUnit_Authenticate_RejectsUnknownMethod(t *testing.T) {
	tr := transportWithMeta(`{"terminal-auth":true}`)
	_, err := tr.Authenticate(context.Background(), libacp.AuthenticateRequest{MethodID: "oauth"})
	require.Error(t, err)
	var e *libacp.Error
	require.ErrorAs(t, err, &e)
	require.Equal(t, libacp.ErrInvalidParams, e.Code)
}

func TestUnit_Authenticate_RejectsTerminalWhenClientLacksCapability(t *testing.T) {
	tr := transportWithMeta("")
	_, err := tr.Authenticate(context.Background(), libacp.AuthenticateRequest{MethodID: terminalAuthMethodID})
	require.Error(t, err, "terminal method must not be honored if it was never advertised to this client")
}

func TestUnit_NewSession_RejectsNonAbsoluteCwd(t *testing.T) {
	tr := transportWithMeta("")
	for _, cwd := range []string{"", "relative/path", "./x"} {
		_, err := tr.NewSession(context.Background(), libacp.NewSessionRequest{Cwd: cwd})
		require.Error(t, err, "cwd %q must be rejected before any side effects", cwd)
		var e *libacp.Error
		require.ErrorAs(t, err, &e)
		require.Equal(t, libacp.ErrInvalidParams, e.Code)
	}
}

func TestUnit_LoadSession_RejectsNonAbsoluteCwd(t *testing.T) {
	tr := transportWithMeta("")
	_, err := tr.LoadSession(context.Background(), libacp.LoadSessionRequest{SessionID: "sess-1", Cwd: "rel"})
	require.Error(t, err)
	var e *libacp.Error
	require.ErrorAs(t, err, &e)
	require.Equal(t, libacp.ErrInvalidParams, e.Code)
}

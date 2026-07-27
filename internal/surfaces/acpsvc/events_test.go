package acpsvc

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/contenox/beam/internal/kernel/taskengine"
	"github.com/contenox/beam/internal/services/localtools"
	"github.com/contenox/beam/libacp"
	"github.com/stretchr/testify/require"
)

// engineEventTranslationMatrix is the translator side of the engine-events
// contract (docs/development/engine-events.md §ACP translation): for every
// TaskEvent kind the engine can emit, a representative event and the exact
// number of ACP notifications this surface renders for it. Driven by
// taskengine.AllTaskEventKinds, so adding an event kind without deciding its
// ACP translation fails here rather than being dropped silently.
var engineEventTranslationMatrix = map[taskengine.TaskEventKind]struct {
	event         taskengine.TaskEvent
	notifications int
}{
	// Chain brackets are engine/journal facts; ACP has no frame for them.
	taskengine.TaskEventChainStarted:   {taskengine.TaskEvent{Kind: taskengine.TaskEventChainStarted, ChainID: "c"}, 0},
	taskengine.TaskEventChainCompleted: {taskengine.TaskEvent{Kind: taskengine.TaskEventChainCompleted, ChainID: "c"}, 0},
	taskengine.TaskEventChainFailed:    {taskengine.TaskEvent{Kind: taskengine.TaskEventChainFailed, ChainID: "c", Error: "boom"}, 0},
	// Suspension (S6) renders nothing: the permission flow's approval card is
	// already on screen and answering it resumes the checkpointed run.
	taskengine.TaskEventChainSuspended: {taskengine.TaskEvent{Kind: taskengine.TaskEventChainSuspended, ChainID: "c", ApprovalID: "call-1"}, 0},
	// Step lifecycle renders as a task card unless the handler is tool-bearing
	// (then the tool events below are the card).
	taskengine.TaskEventStepStarted:   {taskengine.TaskEvent{Kind: taskengine.TaskEventStepStarted, TaskID: "t1", TaskHandler: "noop"}, 1},
	taskengine.TaskEventStepCompleted: {taskengine.TaskEvent{Kind: taskengine.TaskEventStepCompleted, TaskID: "t1", TaskHandler: "noop"}, 1},
	taskengine.TaskEventStepFailed:    {taskengine.TaskEvent{Kind: taskengine.TaskEventStepFailed, TaskID: "t1", TaskHandler: "noop", Error: "boom"}, 1},
	// Prose chunks split into message + thought notifications.
	taskengine.TaskEventStepChunk: {taskengine.TaskEvent{Kind: taskengine.TaskEventStepChunk, TaskHandler: "chat_completion", Content: "hi", Thinking: "hm"}, 2},
	// Stream bracket: consumed, deliberately not rendered (end-of-stream is
	// implicit in ACP; usage indicators come from token_usage).
	taskengine.TaskEventStepStreamEnd: {taskengine.TaskEvent{Kind: taskengine.TaskEventStepStreamEnd, TaskHandler: "chat_completion", ChunkCount: 3, FinishReason: "stop"}, 0},
	// Approval/HITL events are rendered by the permission flow, not this translator.
	taskengine.TaskEventApprovalRequested: {taskengine.TaskEvent{Kind: taskengine.TaskEventApprovalRequested, ApprovalID: "a1", ToolName: "write_file"}, 0},
	taskengine.TaskEventHITLDecision:      {taskengine.TaskEvent{Kind: taskengine.TaskEventHITLDecision, HITLAction: "allow"}, 0},
	taskengine.TaskEventToolCallPending:   {taskengine.TaskEvent{Kind: taskengine.TaskEventToolCallPending, ToolName: "local_fs.read_file", ApprovalID: "call-1"}, 1},
	taskengine.TaskEventToolCall:          {taskengine.TaskEvent{Kind: taskengine.TaskEventToolCall, ToolName: "local_fs.read_file", ApprovalID: "call-1", Content: `"ok"`}, 1},
	taskengine.TaskEventPrint:             {taskengine.TaskEvent{Kind: taskengine.TaskEventPrint, Content: "hello"}, 1},
	taskengine.TaskEventTokenUsage:        {taskengine.TaskEvent{Kind: taskengine.TaskEventTokenUsage, TokenUsed: 100, TokenSize: 1000}, 1},
}

// TestUnit_EngineEventContract_TranslatorConsumesEveryKind drives the native
// event translator (which mirrors Transport.publishEvent case-for-case) with
// one representative event per engine kind and asserts the documented
// translation matrix, including that events with a populated Scope address
// still translate identically (scope is additive; this surface ignores it).
func TestUnit_EngineEventContract_TranslatorConsumesEveryKind(t *testing.T) {
	for _, kind := range taskengine.AllTaskEventKinds() {
		row, ok := engineEventTranslationMatrix[kind]
		require.True(t, ok, "engine event kind %q has no documented ACP translation; update engineEventTranslationMatrix and docs/development/engine-events.md", kind)

		t.Run(string(kind), func(t *testing.T) {
			for _, withScope := range []bool{false, true} {
				ev := row.event
				if withScope {
					ev.Scope = taskengine.EventScope{Chain: "c", Task: "t1", ToolCall: "call-1"}
				}
				payload, err := json.Marshal(ev)
				require.NoError(t, err)

				var got []libacp.SessionNotification
				tr := newNativeEventTranslator(func(_ context.Context, n libacp.SessionNotification) {
					got = append(got, n)
				}, nil)
				tr.publish(context.Background(), libacp.SessionID("sess-1"), payload)
				require.Len(t, got, row.notifications,
					"kind %q (scope populated: %v) must render exactly %d notification(s)", kind, withScope, row.notifications)
			}
		})
	}
}

// TestUnit_EngineEventContract_TransportPublishEventHandlesEveryKind runs the
// connection-bound translator over every kind (scope fields populated) on a
// bare Transport: new engine fields must never break this consumer.
func TestUnit_EngineEventContract_TransportPublishEventHandlesEveryKind(t *testing.T) {
	tr := &Transport{}
	for _, kind := range taskengine.AllTaskEventKinds() {
		row := engineEventTranslationMatrix[kind]
		ev := row.event
		ev.Scope = taskengine.EventScope{Chain: "c", Task: "t1", ToolCall: "call-1"}
		payload, err := json.Marshal(ev)
		require.NoError(t, err)
		require.NotPanics(t, func() {
			tr.publishEvent(context.Background(), libacp.SessionID("sess-1"), payload)
		}, "publishEvent must consume kind %q", kind)
	}
}

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

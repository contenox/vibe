package taskengine_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/contenox/contenox/internal/kernel/taskengine"
	"github.com/contenox/contenox/internal/kernel/tools"
	"github.com/contenox/contenox/internal/models/llmrepo"
	libmodelprovider "github.com/contenox/contenox/internal/models/modelrepo"
	"github.com/contenox/contenox/internal/services/localtools"
	"github.com/contenox/contenox/libtracker"
	"github.com/stretchr/testify/require"
)

func TestUnit_ExecuteToolCalls_CapsOversizedToolResultBeforeChatHistory(t *testing.T) {
	bigPayload := strings.Repeat("x", 4096)
	toolsRepo := tools.NewMockToolsRegistry().
		WithResponse("big", tools.ToolsResponse{Output: map[string]any{"payload": bigPayload}})

	rt := &recordingTracker{}
	exec, err := taskengine.NewExec(context.Background(), &mockModelRepo{}, toolsRepo, rt)
	require.NoError(t, err)

	chainCtx := &taskengine.ChainContext{Tools: map[string]taskengine.ToolWithResolution{
		"big.read": {
			Tool:      taskengine.Tool{Type: "function", Function: taskengine.FunctionTool{Name: "big.read"}},
			ToolsName: "big",
		},
	}}
	history := taskengine.ChatHistory{Messages: []taskengine.Message{{
		Role: "assistant",
		CallTools: []taskengine.ToolCall{{
			ID:   "call-1",
			Type: "function",
			Function: taskengine.FunctionCall{
				Name:      "big.read",
				Arguments: `{}`,
			},
		}},
	}}}

	out, dt, transition, err := exec.TaskExec(
		context.Background(), time.Now().UTC(), 600,
		chainCtx,
		&taskengine.TaskDefinition{ID: "exec", Handler: taskengine.HandleExecuteToolCalls},
		history,
		taskengine.DataTypeChatHistory,
	)
	require.NoError(t, err)
	require.Equal(t, taskengine.DataTypeChatHistory, dt)
	require.Equal(t, taskengine.TransitionToolsExecuted, transition)

	hist := out.(taskengine.ChatHistory)
	last := hist.Messages[len(hist.Messages)-1]
	require.Equal(t, "tool", last.Role)
	require.Equal(t, "call-1", last.ToolCallID)
	require.NotContains(t, last.Content, bigPayload)
	require.Less(t, len(last.Content), 1024)

	var payload map[string]any
	require.NoError(t, json.Unmarshal([]byte(last.Content), &payload))
	require.Equal(t, "tool_result_too_large", payload["error"])
	require.Equal(t, "big.read", payload["tool"])
	require.Equal(t, true, payload["truncated"])
	require.Greater(t, payload["original_bytes"].(float64), float64(4096))
	require.Equal(t, float64(300), payload["max_bytes"])
	require.NotEmpty(t, payload["sha256"])

	changes := rt.changesFor("tool_call")
	require.Contains(t, changes, recordedChange{id: "result_truncated", data: true})
	require.Contains(t, changes, recordedChange{id: "result_max_bytes", data: int64(300)})
}

func TestUnit_ExecuteToolCalls_WriteFileResultCompactButEventCarriesDiff(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "a.txt"), []byte("old\n"), 0644))

	sink := &captureTaskEventSink{}
	constructorCtx := taskengine.WithTaskEventSink(context.Background(), sink)
	exec, err := taskengine.NewExec(constructorCtx, &mockModelRepo{}, localtools.NewLocalFSToolsWith(dir, nil, hostFileIO{}, localtools.LocalFSToolsName, nil), libtracker.NoopTracker{})
	require.NoError(t, err)

	chainCtx := &taskengine.ChainContext{Tools: map[string]taskengine.ToolWithResolution{
		"local_fs.write_file": {
			Tool:      taskengine.Tool{Type: "function", Function: taskengine.FunctionTool{Name: "local_fs.write_file"}},
			ToolsName: "local_fs",
		},
	}}
	history := taskengine.ChatHistory{Messages: []taskengine.Message{{
		Role: "assistant",
		CallTools: []taskengine.ToolCall{{
			ID:   "call-write",
			Type: "function",
			Function: taskengine.FunctionCall{
				Name:      "local_fs.write_file",
				Arguments: `{"path":"a.txt","content":"new\n"}`,
			},
		}},
	}}}

	out, dt, _, err := exec.TaskExec(
		context.Background(), time.Now().UTC(), 4000,
		chainCtx,
		&taskengine.TaskDefinition{ID: "exec", Handler: taskengine.HandleExecuteToolCalls},
		history,
		taskengine.DataTypeChatHistory,
	)
	require.NoError(t, err)
	require.Equal(t, taskengine.DataTypeChatHistory, dt)

	hist := out.(taskengine.ChatHistory)
	last := hist.Messages[len(hist.Messages)-1]
	require.Equal(t, "tool", last.Role)
	require.NotContains(t, last.Content, "old_text")
	require.NotContains(t, last.Content, "new_text")
	require.NotContains(t, last.Content, "old\\n")
	require.NotContains(t, last.Content, "new\\n")

	var compact map[string]any
	require.NoError(t, json.Unmarshal([]byte(last.Content), &compact))
	require.Equal(t, true, compact["written"])
	require.Equal(t, float64(4), compact["old_bytes"])
	require.Equal(t, float64(4), compact["new_bytes"])
	require.NotEmpty(t, compact["old_sha256"])
	require.NotEmpty(t, compact["new_sha256"])

	var completed taskengine.TaskEvent
	for _, ev := range sink.events {
		if ev.Kind == taskengine.TaskEventToolCall && ev.ApprovalID == "call-write" {
			completed = ev
		}
	}
	require.Equal(t, taskengine.TaskEventToolCall, completed.Kind)
	require.Equal(t, filepath.Join(dir, "a.txt"), completed.ToolDiffPath)
	require.Equal(t, "old\n", completed.ToolDiffOldText)
	require.Equal(t, "new\n", completed.ToolDiffNewText)
}

func TestUnit_ExecuteToolCalls_InvalidToolCallGetsTelemetrySpan(t *testing.T) {
	rt := &recordingTracker{}
	exec, err := taskengine.NewExec(context.Background(), &mockModelRepo{}, tools.NewMockToolsRegistry(), rt)
	require.NoError(t, err)

	history := taskengine.ChatHistory{Messages: []taskengine.Message{{
		Role: "assistant",
		CallTools: []taskengine.ToolCall{{
			ID:   "missing-call",
			Type: "function",
			Function: taskengine.FunctionCall{
				Name:      "missing.tool",
				Arguments: `{}`,
			},
		}},
	}}}

	out, dt, transition, err := exec.TaskExec(
		context.Background(), time.Now().UTC(), 4000,
		&taskengine.ChainContext{},
		&taskengine.TaskDefinition{ID: "exec", Handler: taskengine.HandleExecuteToolCalls},
		history,
		taskengine.DataTypeChatHistory,
	)
	require.NoError(t, err)
	require.Equal(t, taskengine.DataTypeChatHistory, dt)
	require.Equal(t, taskengine.TransitionNoCallsFound, transition)

	hist := out.(taskengine.ChatHistory)
	last := hist.Messages[len(hist.Messages)-1]
	require.Equal(t, "tool", last.Role)
	require.Contains(t, last.Content, "not found")

	span := rt.find("tool_call")
	require.NotNil(t, span)
	require.Equal(t, 1, span.errs)
	require.Equal(t, 1, span.ended)
	require.Contains(t, rt.changesFor("tool_call"), recordedChange{id: "invalid_call_class", data: "not_found"})
}

func TestUnit_ChatCompletion_ReportsToolSchemaBytes(t *testing.T) {
	rt := &recordingTracker{}
	repo := &mockModelRepo{
		chatFunc: func(context.Context, llmrepo.Request, []libmodelprovider.Message, ...libmodelprovider.ChatArgument) (libmodelprovider.ChatResult, llmrepo.Meta, error) {
			return libmodelprovider.ChatResult{
				Message: libmodelprovider.Message{Role: "assistant", Content: "done"},
			}, llmrepo.Meta{ModelName: "test-model"}, nil
		},
	}
	toolsRepo := &scopedExecToolsRepo{supported: []string{"local_fs"}}
	exec, err := taskengine.NewExec(context.Background(), repo, toolsRepo, rt)
	require.NoError(t, err)

	chainCtx := &taskengine.ChainContext{Tools: map[string]taskengine.ToolWithResolution{
		"local_fs.read_file": {
			Tool: taskengine.Tool{Type: "function", Function: taskengine.FunctionTool{
				Name:        "local_fs.read_file",
				Description: "read a file",
				Parameters:  map[string]any{"type": "object", "properties": map[string]any{"path": map[string]any{"type": "string"}}},
			}},
			ToolsName: "local_fs",
		},
	}}

	_, _, _, err = exec.TaskExec(
		context.Background(), time.Now().UTC(), 4000,
		chainCtx,
		&taskengine.TaskDefinition{
			ID:      "chat",
			Handler: taskengine.HandleChatCompletion,
			ExecuteConfig: &taskengine.LLMExecutionConfig{
				Model: "test-model",
				Tools: []string{"local_fs"},
			},
		},
		"hello",
		taskengine.DataTypeString,
	)
	require.NoError(t, err)

	var toolsPrepared map[string]any
	for _, change := range rt.changesFor("SimpleExec") {
		if change.id == "tools_prepared" {
			toolsPrepared = change.data.(map[string]any)
		}
	}
	require.NotNil(t, toolsPrepared)
	require.Equal(t, 1, toolsPrepared["count"])
	require.Greater(t, toolsPrepared["schema_bytes"].(int), 0)
}

func TestUnit_ExecuteToolCalls_ReportsRepeatedSameToolAndArguments(t *testing.T) {
	toolsRepo := tools.NewMockToolsRegistry().
		WithResponse("echo", tools.ToolsResponse{Output: "ok"})

	rt := &recordingTracker{}
	exec, err := taskengine.NewExec(context.Background(), &mockModelRepo{}, toolsRepo, rt)
	require.NoError(t, err)

	chainCtx := &taskengine.ChainContext{Tools: map[string]taskengine.ToolWithResolution{
		"echo.echo": {
			Tool:      taskengine.Tool{Type: "function", Function: taskengine.FunctionTool{Name: "echo.echo"}},
			ToolsName: "echo",
		},
	}}
	history := taskengine.ChatHistory{Messages: []taskengine.Message{
		{
			Role: "assistant",
			CallTools: []taskengine.ToolCall{{
				ID:   "prior",
				Type: "function",
				Function: taskengine.FunctionCall{
					Name:      "echo.echo",
					Arguments: `{ "value": "same" }`,
				},
			}},
		},
		{Role: "tool", ToolCallID: "prior", Content: `"ok"`},
		{
			Role: "assistant",
			CallTools: []taskengine.ToolCall{{
				ID:   "again",
				Type: "function",
				Function: taskengine.FunctionCall{
					Name:      "echo.echo",
					Arguments: `{"value":"same"}`,
				},
			}},
		},
	}}

	_, _, _, err = exec.TaskExec(
		context.Background(), time.Now().UTC(), 4000,
		chainCtx,
		&taskengine.TaskDefinition{ID: "exec", Handler: taskengine.HandleExecuteToolCalls},
		history,
		taskengine.DataTypeChatHistory,
	)
	require.NoError(t, err)

	changes := rt.changesFor("tool_call")
	require.Contains(t, changes, recordedChange{id: "repeat_index", data: 2})
	require.Contains(t, changes, recordedChange{id: "repeated_call", data: true})
}

type hostFileIO struct{}

func (hostFileIO) ReadFile(_ context.Context, path string) ([]byte, error) { return os.ReadFile(path) }

func (hostFileIO) WriteFile(_ context.Context, path string, data []byte) error {
	return os.WriteFile(path, data, 0o644)
}

package taskengine_test

import (
	"context"
	"testing"
	"time"

	"github.com/contenox/contenox/internal/kernel/taskengine"
	"github.com/contenox/contenox/internal/kernel/tools"
	"github.com/contenox/contenox/libtracker"
	"github.com/contenox/contenox/internal/models/llmrepo"
	libmodelprovider "github.com/contenox/contenox/internal/models/modelrepo"
	"github.com/stretchr/testify/require"
)

// TestUnit_DanglingToolCallGuard_FlushesAndFallsThrough verifies that when
// HandleChatCompletion receives a ChatHistory whose last message is an unanswered
// assistant tool_call (the state-machine "budget handoff" scenario), the guard:
//  1. executes the pending tool inline (via the toolsProvider),
//  2. appends the tool result to history,
//  3. falls through to executeLLM so the task's SystemInstruction is still
//     injected and the LLM gets a real turn with the tool result in context.
//
// This is the regression test for the bug where a budget transition shunted the
// chain to a new chat task whose recovery_chat ran for ~2ms (just the tool flush)
// and the BUDGET system_instruction never reached the model.
func TestUnit_DanglingToolCallGuard_FlushesAndFallsThrough(t *testing.T) {
	toolsRepo := tools.NewMockToolsRegistry().
		WithResponse("echo", tools.ToolsResponse{Output: "ECHOED"})

	var capturedMessages []libmodelprovider.Message
	repo := &mockModelRepo{
		chatFunc: func(_ context.Context, _ llmrepo.Request, messages []libmodelprovider.Message, _ ...libmodelprovider.ChatArgument) (libmodelprovider.ChatResult, llmrepo.Meta, error) {
			capturedMessages = messages
			return libmodelprovider.ChatResult{
				Message: libmodelprovider.Message{Role: "assistant", Content: "all good"},
			}, llmrepo.Meta{ModelName: "test-model"}, nil
		},
	}

	exec, err := taskengine.NewExec(context.Background(), repo, toolsRepo, libtracker.NoopTracker{})
	require.NoError(t, err)

	chainCtx := &taskengine.ChainContext{
		Tools: map[string]taskengine.ToolWithResolution{
			"echo.echo": {
				Tool:      taskengine.Tool{Type: "function", Function: taskengine.FunctionTool{Name: "echo.echo"}},
				ToolsName: "echo",
			},
		},
	}

	dangling := taskengine.ChatHistory{
		Messages: []taskengine.Message{
			{Role: "user", Content: "do it"},
			{
				Role: "assistant",
				CallTools: []taskengine.ToolCall{
					{ID: "call-1", Type: "function", Function: taskengine.FunctionCall{Name: "echo.echo", Arguments: `{"input":"hi"}`}},
				},
			},
		},
	}

	task := &taskengine.TaskDefinition{
		ID:                "recovery_chat",
		Handler:           taskengine.HandleChatCompletion,
		SystemInstruction: "BUDGET: round 10/20 — be efficient",
		ExecuteConfig: &taskengine.LLMExecutionConfig{
			Model: "test-model",
			Tools: []string{"echo"},
		},
	}

	out, dt, _, err := exec.TaskExec(
		context.Background(), time.Now().UTC(), 4000,
		chainCtx, task, dangling, taskengine.DataTypeChatHistory)
	require.NoError(t, err)
	require.Equal(t, taskengine.DataTypeChatHistory, dt)

	require.Equal(t, 1, toolsRepo.CallCount(), "guard must execute the dangling tool exactly once")
	require.NotNil(t, toolsRepo.LastCall())
	require.Equal(t, "echo", toolsRepo.LastCall().Args.Name)

	require.NotNil(t, capturedMessages, "executeLLM must be reached after the flush — Chat was not invoked")

	var sawSystem, sawAssistantToolCall, sawToolResult bool
	for _, m := range capturedMessages {
		switch m.Role {
		case "system":
			if m.Content == "BUDGET: round 10/20 — be efficient" {
				sawSystem = true
			}
		case "assistant":
			if len(m.ToolCalls) > 0 && m.ToolCalls[0].ID == "call-1" {
				sawAssistantToolCall = true
			}
		case "tool":
			if m.ToolCallID == "call-1" {
				sawToolResult = true
			}
		}
	}
	require.True(t, sawSystem, "SystemInstruction must be injected before the LLM call so the BUDGET prompt actually fires")
	require.True(t, sawAssistantToolCall, "the original assistant tool_call must remain in history for the model to see")
	require.True(t, sawToolResult, "the synthesized tool result must be in history — otherwise the provider rejects the dangling call")

	hist, ok := out.(taskengine.ChatHistory)
	require.True(t, ok)
	last := hist.Messages[len(hist.Messages)-1]
	require.Equal(t, "assistant", last.Role)
	require.Equal(t, "all good", last.Content, "the LLM's recovery response should be the final message")
}

// TestUnit_DanglingToolCallGuard_NoOpWhenNoToolCalls verifies the guard does NOT
// trigger when the last message is a normal user/assistant text — i.e. the normal
// chat completion path is unaffected by the new guard logic.
func TestUnit_DanglingToolCallGuard_NoOpWhenNoToolCalls(t *testing.T) {
	toolsRepo := tools.NewMockToolsRegistry()

	chatInvoked := false
	repo := &mockModelRepo{
		chatFunc: func(_ context.Context, _ llmrepo.Request, _ []libmodelprovider.Message, _ ...libmodelprovider.ChatArgument) (libmodelprovider.ChatResult, llmrepo.Meta, error) {
			chatInvoked = true
			return libmodelprovider.ChatResult{
				Message: libmodelprovider.Message{Role: "assistant", Content: "hello"},
			}, llmrepo.Meta{ModelName: "test-model"}, nil
		},
	}

	exec, err := taskengine.NewExec(context.Background(), repo, toolsRepo, libtracker.NoopTracker{})
	require.NoError(t, err)

	chainCtx := &taskengine.ChainContext{
		Tools: map[string]taskengine.ToolWithResolution{
			"echo.echo": {
				Tool:      taskengine.Tool{Type: "function", Function: taskengine.FunctionTool{Name: "echo.echo"}},
				ToolsName: "echo",
			},
		},
	}

	hist := taskengine.ChatHistory{
		Messages: []taskengine.Message{
			{Role: "user", Content: "hi"},
		},
	}

	task := &taskengine.TaskDefinition{
		ID:      "chat",
		Handler: taskengine.HandleChatCompletion,
		ExecuteConfig: &taskengine.LLMExecutionConfig{
			Model: "test-model",
			Tools: []string{"echo"},
		},
	}

	_, _, _, err = exec.TaskExec(
		context.Background(), time.Now().UTC(), 4000,
		chainCtx, task, hist, taskengine.DataTypeChatHistory)
	require.NoError(t, err)

	require.Equal(t, 0, toolsRepo.CallCount(), "guard must not run any tools when there is no dangling tool_call")
	require.True(t, chatInvoked, "the normal chat path must still reach the LLM")
}

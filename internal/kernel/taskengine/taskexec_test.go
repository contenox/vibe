package taskengine_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/contenox/beam/internal/kernel/taskengine"
	"github.com/contenox/beam/internal/kernel/tools"
	"github.com/contenox/beam/internal/libtracker"
	"github.com/contenox/beam/internal/models/llmrepo"
	libmodelprovider "github.com/contenox/beam/internal/models/modelrepo"
	"github.com/stretchr/testify/require"
)

func TestUnit_TaskExec_ChatCompletionRejectsNilInput(t *testing.T) {
	repo := &mockModelRepo{
		chatFunc: func(_ context.Context, _ llmrepo.Request, _ []libmodelprovider.Message, _ ...libmodelprovider.ChatArgument) (libmodelprovider.ChatResult, llmrepo.Meta, error) {
			return libmodelprovider.ChatResult{}, llmrepo.Meta{}, errors.New("provider should not be called")
		},
	}
	toolsRepo := tools.NewMockToolsRegistry().
		WithResponse("echo", tools.ToolsResponse{Output: "ok"})
	exec, err := taskengine.NewExec(context.Background(), repo, toolsRepo, libtracker.NoopTracker{})
	require.NoError(t, err)

	task := &taskengine.TaskDefinition{
		ID:            "acp_chat",
		Handler:       taskengine.HandleChatCompletion,
		ExecuteConfig: &taskengine.LLMExecutionConfig{Model: "test-model"},
	}

	_, _, _, err = exec.TaskExec(context.Background(), time.Now().UTC(), 1000, &taskengine.ChainContext{}, task, nil, taskengine.DataTypeAny)
	require.Error(t, err)
	require.Contains(t, err.Error(), "input is nil for task acp_chat")
}

func TestUnit_TaskExec_ChatCompletionAddsNoToolsGuardWhenRequestedToolsResolveEmpty(t *testing.T) {
	var seenMessages []libmodelprovider.Message
	var seenToolCount int
	repo := &mockModelRepo{
		chatFunc: func(_ context.Context, _ llmrepo.Request, messages []libmodelprovider.Message, opts ...libmodelprovider.ChatArgument) (libmodelprovider.ChatResult, llmrepo.Meta, error) {
			cfg := &libmodelprovider.ChatConfig{}
			for _, opt := range opts {
				opt.Apply(cfg)
			}
			seenToolCount = len(cfg.Tools)
			seenMessages = append([]libmodelprovider.Message(nil), messages...)
			return libmodelprovider.ChatResult{
				Message: libmodelprovider.Message{Role: "assistant", Content: "no tools answer"},
			}, llmrepo.Meta{ModelName: "test-model", ProviderType: "llama"}, nil
		},
	}
	exec, err := taskengine.NewExec(context.Background(), repo, tools.NewMockToolsRegistry(), libtracker.NoopTracker{})
	require.NoError(t, err)

	task := &taskengine.TaskDefinition{
		ID:                "chat",
		Handler:           taskengine.HandleChatCompletion,
		SystemInstruction: "Use tools when they help.",
		ExecuteConfig: &taskengine.LLMExecutionConfig{
			Model: "test-model",
			Tools: []string{"*"},
		},
	}

	_, _, _, err = exec.TaskExec(
		context.Background(), time.Now().UTC(), 4000,
		&taskengine.ChainContext{}, task,
		taskengine.ChatHistory{Messages: []taskengine.Message{{Role: "user", Content: "inspect this"}}},
		taskengine.DataTypeChatHistory,
	)
	require.NoError(t, err)
	require.Zero(t, seenToolCount)
	require.NotEmpty(t, seenMessages)
	require.Equal(t, "system", seenMessages[0].Role)
	require.Contains(t, seenMessages[0].Content, "No tools are available in this turn")
}

func TestUnit_TaskExec_ChatCompletionUsesRequestedContextLengthMinimum(t *testing.T) {
	var seenReq llmrepo.Request
	repo := &mockModelRepo{
		chatFunc: func(_ context.Context, req llmrepo.Request, _ []libmodelprovider.Message, _ ...libmodelprovider.ChatArgument) (libmodelprovider.ChatResult, llmrepo.Meta, error) {
			seenReq = req
			return libmodelprovider.ChatResult{
				Message: libmodelprovider.Message{Role: "assistant", Content: "ok"},
			}, llmrepo.Meta{ModelName: "test-model", ProviderType: "llama"}, nil
		},
	}
	exec, err := taskengine.NewExec(context.Background(), repo, tools.NewMockToolsRegistry(), libtracker.NoopTracker{})
	require.NoError(t, err)

	task := &taskengine.TaskDefinition{
		ID:            "chat",
		Handler:       taskengine.HandleChatCompletion,
		ExecuteConfig: &taskengine.LLMExecutionConfig{Model: "test-model"},
	}
	ctx := taskengine.WithRequestedContextLength(context.Background(), 4096)
	_, _, _, err = exec.TaskExec(
		ctx, time.Now().UTC(), 131072,
		&taskengine.ChainContext{}, task,
		taskengine.ChatHistory{Messages: []taskengine.Message{{Role: "user", Content: "hello"}}},
		taskengine.DataTypeChatHistory,
	)
	require.NoError(t, err)
	require.Equal(t, 4096, seenReq.ContextLength)
}

func TestUnit_TaskExec_RouteUsesRequestedContextLengthMinimum(t *testing.T) {
	var seenReq llmrepo.Request
	repo := &mockModelRepo{
		promptFunc: func(_ context.Context, req llmrepo.Request, _ string, _ float32, _ string) (string, llmrepo.Meta, error) {
			seenReq = req
			return "general", llmrepo.Meta{ModelName: "test-model", ProviderType: "llama"}, nil
		},
	}
	exec, err := taskengine.NewExec(context.Background(), repo, tools.NewMockToolsRegistry(), libtracker.NoopTracker{})
	require.NoError(t, err)

	task := &taskengine.TaskDefinition{
		ID:            "route",
		Handler:       taskengine.HandleRoute,
		ExecuteConfig: &taskengine.LLMExecutionConfig{Model: "test-model"},
		Transition: taskengine.TaskTransition{Branches: []taskengine.TransitionBranch{
			{Operator: taskengine.OpEquals, When: "general", Goto: "chat"},
			{Operator: taskengine.OpEquals, When: "coding_change", Goto: "coding"},
		}},
	}
	ctx := taskengine.WithRequestedContextLength(context.Background(), 8192)
	_, _, eval, err := exec.TaskExec(ctx, time.Now().UTC(), 131072, &taskengine.ChainContext{}, task, "hello", taskengine.DataTypeString)
	require.NoError(t, err)
	require.Equal(t, "general", eval)
	require.Equal(t, 8192, seenReq.ContextLength)
}

func TestUnit_TaskExec_ChatCompletionRetriesWithoutToolsWhenProviderRejectsToolCalls(t *testing.T) {
	var seenToolNames [][]string
	var secondMessages []libmodelprovider.Message
	repo := &mockModelRepo{
		chatFunc: func(_ context.Context, _ llmrepo.Request, messages []libmodelprovider.Message, opts ...libmodelprovider.ChatArgument) (libmodelprovider.ChatResult, llmrepo.Meta, error) {
			cfg := &libmodelprovider.ChatConfig{}
			for _, opt := range opts {
				opt.Apply(cfg)
			}
			var names []string
			for _, tool := range cfg.Tools {
				if tool.Function != nil {
					names = append(names, tool.Function.Name)
				}
			}
			seenToolNames = append(seenToolNames, names)
			if len(cfg.Tools) > 0 {
				return libmodelprovider.ChatResult{}, llmrepo.Meta{}, errors.New("chat execution failed: llama: unsupported feature: tool calls (model declares no tool_calls.protocol)")
			}
			secondMessages = append([]libmodelprovider.Message(nil), messages...)
			return libmodelprovider.ChatResult{
				Message: libmodelprovider.Message{Role: "assistant", Content: "hello"},
			}, llmrepo.Meta{ModelName: "test-model", ProviderType: "llama"}, nil
		},
	}
	toolsRepo := tools.NewMockToolsRegistry().
		WithResponse("echo", tools.ToolsResponse{Output: "ok"})
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
	history := taskengine.ChatHistory{Messages: []taskengine.Message{
		{Role: "user", Content: "old request"},
		{Role: "assistant", CallTools: []taskengine.ToolCall{{
			ID: "call-1", Type: "function", Function: taskengine.FunctionCall{Name: "echo.echo", Arguments: `{}`},
		}}},
		{Role: "tool", ToolCallID: "call-1", Content: "ok"},
		{Role: "user", Content: "say hi"},
	}}
	task := &taskengine.TaskDefinition{
		ID:      "chat",
		Handler: taskengine.HandleChatCompletion,
		ExecuteConfig: &taskengine.LLMExecutionConfig{
			Model: "test-model",
			Tools: []string{"echo"},
		},
	}

	out, dt, transition, err := exec.TaskExec(
		context.Background(), time.Now().UTC(), 4000,
		chainCtx, task, history, taskengine.DataTypeChatHistory,
	)
	require.NoError(t, err)
	require.Equal(t, taskengine.DataTypeChatHistory, dt)
	require.Equal(t, taskengine.TransitionExecuted, transition)
	require.Len(t, seenToolNames, 2)
	require.Equal(t, []string{"echo.echo"}, seenToolNames[0])
	require.Empty(t, seenToolNames[1])
	require.NotEmpty(t, secondMessages)
	for _, msg := range secondMessages {
		require.NotEqual(t, "tool", msg.Role)
		require.Empty(t, msg.ToolCalls)
		require.Empty(t, msg.ToolCallID)
		if msg.Role == "assistant" {
			require.NotEmpty(t, msg.Content)
		}
	}
	hist, ok := out.(taskengine.ChatHistory)
	require.True(t, ok)
	require.Equal(t, "hello", hist.Messages[len(hist.Messages)-1].Content)
}

func TestUnit_TaskExec_ChatCompletionRetriesWithoutToolsWhenToolSchemaRejected(t *testing.T) {
	var seenToolCounts []int
	repo := &mockModelRepo{
		chatFunc: func(_ context.Context, _ llmrepo.Request, _ []libmodelprovider.Message, opts ...libmodelprovider.ChatArgument) (libmodelprovider.ChatResult, llmrepo.Meta, error) {
			cfg := &libmodelprovider.ChatConfig{}
			for _, opt := range opts {
				opt.Apply(cfg)
			}
			seenToolCounts = append(seenToolCounts, len(cfg.Tools))
			if len(cfg.Tools) > 0 {
				return libmodelprovider.ChatResult{}, llmrepo.Meta{}, errors.New(`chat execution failed: rpc error: code = Internal desc = internal: llamasession: apply chat template: llamacppshim: common chat template: JSON schema conversion failed:
Unrecognized schema: {"description":"Request body"}`)
			}
			return libmodelprovider.ChatResult{
				Message: libmodelprovider.Message{Role: "assistant", Content: "hello"},
			}, llmrepo.Meta{ModelName: "test-model", ProviderType: "llama"}, nil
		},
	}
	toolsRepo := tools.NewMockToolsRegistry().
		WithResponse("echo", tools.ToolsResponse{Output: "ok"})
	exec, err := taskengine.NewExec(context.Background(), repo, toolsRepo, libtracker.NoopTracker{})
	require.NoError(t, err)

	chainCtx := &taskengine.ChainContext{
		Tools: map[string]taskengine.ToolWithResolution{
			"echo.web_post": {
				Tool: taskengine.Tool{Type: "function", Function: taskengine.FunctionTool{
					Name:       "echo.web_post",
					Parameters: map[string]any{"type": "object", "properties": map[string]any{"body": map[string]any{"description": "Request body"}}},
				}},
				ToolsName: "echo",
			},
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

	out, dt, transition, err := exec.TaskExec(
		context.Background(), time.Now().UTC(), 4000,
		chainCtx, task, taskengine.ChatHistory{Messages: []taskengine.Message{{Role: "user", Content: "say hi"}}}, taskengine.DataTypeChatHistory,
	)
	require.NoError(t, err)
	require.Equal(t, taskengine.DataTypeChatHistory, dt)
	require.Equal(t, taskengine.TransitionExecuted, transition)
	require.Equal(t, []int{1, 0}, seenToolCounts)
	hist, ok := out.(taskengine.ChatHistory)
	require.True(t, ok)
	require.Equal(t, "hello", hist.Messages[len(hist.Messages)-1].Content)
}

func TestUnit_TaskExec_ChatCompletionRetriesWithoutToolsWhenToolsOverflowContext(t *testing.T) {
	var seenToolCounts []int
	repo := &mockModelRepo{
		chatFunc: func(_ context.Context, _ llmrepo.Request, _ []libmodelprovider.Message, opts ...libmodelprovider.ChatArgument) (libmodelprovider.ChatResult, llmrepo.Meta, error) {
			cfg := &libmodelprovider.ChatConfig{}
			for _, opt := range opts {
				opt.Apply(cfg)
			}
			seenToolCounts = append(seenToolCounts, len(cfg.Tools))
			if len(cfg.Tools) > 0 {
				return libmodelprovider.ChatResult{}, llmrepo.Meta{}, errors.New("chat execution failed: llama: context overflow: exceeded the session context window: exceeded the session context window during suffix: resident_tokens=1 additional_tokens=4871 num_ctx=4074")
			}
			return libmodelprovider.ChatResult{
				Message: libmodelprovider.Message{Role: "assistant", Content: "hello"},
			}, llmrepo.Meta{ModelName: "test-model", ProviderType: "llama"}, nil
		},
	}
	toolsRepo := tools.NewMockToolsRegistry().
		WithResponse("echo", tools.ToolsResponse{Output: "ok"})
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
	task := &taskengine.TaskDefinition{
		ID:      "chat",
		Handler: taskengine.HandleChatCompletion,
		ExecuteConfig: &taskengine.LLMExecutionConfig{
			Model: "test-model",
			Tools: []string{"echo"},
		},
	}

	out, dt, transition, err := exec.TaskExec(
		context.Background(), time.Now().UTC(), 4000,
		chainCtx, task, taskengine.ChatHistory{Messages: []taskengine.Message{{Role: "user", Content: "say hi"}}}, taskengine.DataTypeChatHistory,
	)
	require.NoError(t, err)
	require.Equal(t, taskengine.DataTypeChatHistory, dt)
	require.Equal(t, taskengine.TransitionExecuted, transition)
	require.Equal(t, []int{1, 0}, seenToolCounts)
	hist, ok := out.(taskengine.ChatHistory)
	require.True(t, ok)
	require.Equal(t, "hello", hist.Messages[len(hist.Messages)-1].Content)
}

// Regression: an early break in the execute_tool_calls batch (invalid
// arguments on the first call) must not leave the remaining calls without a
// result — strict providers reject transcripts with unanswered tool calls.
func TestUnit_TaskExec_ExecuteToolCallsAnswersWholeBatchOnEarlyBreak(t *testing.T) {
	repo := &mockModelRepo{}
	toolsRepo := tools.NewMockToolsRegistry().
		WithResponse("echo", tools.ToolsResponse{Output: "ok"})
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
	history := taskengine.ChatHistory{Messages: []taskengine.Message{
		{Role: "user", Content: "do two things"},
		{Role: "assistant", CallTools: []taskengine.ToolCall{
			{ID: "call-1", Type: "function", Function: taskengine.FunctionCall{Name: "echo.echo", Arguments: `{not-json`}},
			{ID: "call-2", Type: "function", Function: taskengine.FunctionCall{Name: "echo.echo", Arguments: `{}`}},
		}},
	}}
	task := &taskengine.TaskDefinition{
		ID:      "run_tools",
		Handler: taskengine.HandleExecuteToolCalls,
	}

	out, _, _, taskErr := exec.TaskExec(
		context.Background(), time.Now().UTC(), 4000,
		chainCtx, task, history, taskengine.DataTypeChatHistory,
	)
	require.Error(t, taskErr)

	hist, ok := out.(taskengine.ChatHistory)
	require.True(t, ok)
	answered := map[string]bool{}
	for _, m := range hist.Messages {
		if m.Role == "tool" {
			answered[m.ToolCallID] = true
		}
	}
	require.True(t, answered["call-1"], "failed call must carry an error result")
	require.True(t, answered["call-2"], "call after the break must carry a stub result")
}

// Regression: recovery/summarise tasks receive an earlier task's output via
// input_var, which can end mid tool-call protocol (unanswered call, orphaned
// result). chat_completion must reconcile before the provider sees it.
func TestUnit_TaskExec_ChatCompletionRepairsDanglingToolProtocol(t *testing.T) {
	var seenMessages []libmodelprovider.Message
	repo := &mockModelRepo{
		chatFunc: func(_ context.Context, _ llmrepo.Request, messages []libmodelprovider.Message, _ ...libmodelprovider.ChatArgument) (libmodelprovider.ChatResult, llmrepo.Meta, error) {
			seenMessages = append([]libmodelprovider.Message(nil), messages...)
			return libmodelprovider.ChatResult{
				Message: libmodelprovider.Message{Role: "assistant", Content: "recovered"},
			}, llmrepo.Meta{ModelName: "test-model", ProviderType: "llama"}, nil
		},
	}
	exec, err := taskengine.NewExec(context.Background(), repo, tools.NewMockToolsRegistry(), libtracker.NoopTracker{})
	require.NoError(t, err)

	history := taskengine.ChatHistory{Messages: []taskengine.Message{
		{Role: "user", Content: "hello"},
		{Role: "tool", ToolCallID: "ghost", Content: "orphaned result"},
		{Role: "assistant", CallTools: []taskengine.ToolCall{{
			ID: "dangling", Type: "function", Function: taskengine.FunctionCall{Name: "list_dir", Arguments: `{}`},
		}}},
	}}
	task := &taskengine.TaskDefinition{
		ID:            "recovery_chat",
		Handler:       taskengine.HandleChatCompletion,
		ExecuteConfig: &taskengine.LLMExecutionConfig{Model: "test-model"},
	}

	_, _, _, err = exec.TaskExec(
		context.Background(), time.Now().UTC(), 4000,
		&taskengine.ChainContext{}, task, history, taskengine.DataTypeChatHistory,
	)
	require.NoError(t, err)
	require.NotEmpty(t, seenMessages)

	var danglingAnswered bool
	for _, m := range seenMessages {
		if m.Role == "tool" {
			require.NotEqual(t, "ghost", m.ToolCallID, "orphaned tool result must not reach the provider")
			if m.ToolCallID == "dangling" {
				danglingAnswered = true
			}
		}
	}
	require.True(t, danglingAnswered, "unanswered tool call must be paired with a stub result before the provider sees it")
}

package taskengine_test

import (
	"context"
	"testing"
	"time"

	"github.com/contenox/beam/internal/kernel/taskengine"
	"github.com/contenox/beam/internal/libtracker"
	"github.com/getkin/kin-openapi/openapi3"
	"github.com/stretchr/testify/require"
)

type scopedExecToolsRepo struct {
	supported []string
	calls     []taskengine.ToolsCall
}

func (s *scopedExecToolsRepo) Exec(_ context.Context, _ time.Time, _ any, _ bool, args *taskengine.ToolsCall) (any, taskengine.DataType, error) {
	s.calls = append(s.calls, *args)
	return "ok", taskengine.DataTypeString, nil
}

func (s *scopedExecToolsRepo) Supports(context.Context) ([]string, error) {
	return append([]string(nil), s.supported...), nil
}

func (s *scopedExecToolsRepo) GetSchemasForSupportedTools(context.Context) (map[string]*openapi3.T, error) {
	return map[string]*openapi3.T{}, nil
}

func (s *scopedExecToolsRepo) GetToolsForToolsByName(_ context.Context, name string) ([]taskengine.Tool, error) {
	for _, supported := range s.supported {
		if supported == name {
			return []taskengine.Tool{{Type: "function", Function: taskengine.FunctionTool{Name: "tool"}}}, nil
		}
	}
	return nil, taskengine.ErrToolsNotFound
}

func runExecuteToolScope(t *testing.T, cfg *taskengine.LLMExecutionConfig, toolName string) (taskengine.ChatHistory, string, *scopedExecToolsRepo) {
	t.Helper()

	repo := &scopedExecToolsRepo{supported: []string{"local_fs", "local_shell"}}
	exec, err := taskengine.NewExec(context.Background(), &mockModelRepo{}, repo, libtracker.NoopTracker{})
	require.NoError(t, err)

	chainCtx := &taskengine.ChainContext{Tools: map[string]taskengine.ToolWithResolution{
		"local_fs.read_file": {
			Tool:      taskengine.Tool{Type: "function", Function: taskengine.FunctionTool{Name: "local_fs.read_file"}},
			ToolsName: "local_fs",
		},
		"local_fs.write_file": {
			Tool:      taskengine.Tool{Type: "function", Function: taskengine.FunctionTool{Name: "local_fs.write_file"}},
			ToolsName: "local_fs",
		},
		"local_shell.local_shell": {
			Tool:      taskengine.Tool{Type: "function", Function: taskengine.FunctionTool{Name: "local_shell.local_shell"}},
			ToolsName: "local_shell",
		},
	}}

	history := taskengine.ChatHistory{Messages: []taskengine.Message{{
		Role: "assistant",
		CallTools: []taskengine.ToolCall{{
			ID:   "call-1",
			Type: "function",
			Function: taskengine.FunctionCall{
				Name:      toolName,
				Arguments: `{}`,
			},
		}},
	}}}

	out, dt, transition, err := exec.TaskExec(
		context.Background(), time.Now().UTC(), 4000,
		chainCtx,
		&taskengine.TaskDefinition{ID: "exec", Handler: taskengine.HandleExecuteToolCalls, ExecuteConfig: cfg},
		history,
		taskengine.DataTypeChatHistory,
	)
	require.NoError(t, err)
	require.Equal(t, taskengine.DataTypeChatHistory, dt)
	hist, ok := out.(taskengine.ChatHistory)
	require.True(t, ok)
	return hist, transition, repo
}

func TestUnit_ExecuteToolCalls_ExplicitToolsScope_AllowsProvider(t *testing.T) {
	_, transition, repo := runExecuteToolScope(t, &taskengine.LLMExecutionConfig{Tools: []string{"local_fs"}}, "local_fs.read_file")

	require.Equal(t, taskengine.TransitionToolsExecuted, transition)
	require.Len(t, repo.calls, 1)
	require.Equal(t, "local_fs", repo.calls[0].Name)
	require.Equal(t, "read_file", repo.calls[0].ToolName)
}

func TestUnit_ExecuteToolCalls_UnqualifiedUniqueLeafResolves(t *testing.T) {
	_, transition, repo := runExecuteToolScope(t, &taskengine.LLMExecutionConfig{Tools: []string{"local_fs"}}, "write_file")

	require.Equal(t, taskengine.TransitionToolsExecuted, transition)
	require.Len(t, repo.calls, 1)
	require.Equal(t, "local_fs", repo.calls[0].Name)
	require.Equal(t, "write_file", repo.calls[0].ToolName)
}

func TestUnit_ExecuteToolCalls_ExplicitToolsScope_BlocksProviderOutsideScope(t *testing.T) {
	hist, _, repo := runExecuteToolScope(t, &taskengine.LLMExecutionConfig{Tools: []string{"local_fs"}}, "local_shell.local_shell")

	require.Empty(t, repo.calls)
	last := hist.Messages[len(hist.Messages)-1]
	require.Equal(t, "tool", last.Role)
	require.Contains(t, last.Content, "not allowed")
	require.Equal(t, "call-1", last.ToolCallID)
}

func TestUnit_ExecuteToolCalls_HideTools_BlocksNamespacedTool(t *testing.T) {
	hist, _, repo := runExecuteToolScope(t, &taskengine.LLMExecutionConfig{
		Tools:     []string{"local_fs"},
		HideTools: []string{"local_fs.write_file"},
	}, "local_fs.write_file")

	require.Empty(t, repo.calls)
	last := hist.Messages[len(hist.Messages)-1]
	require.Equal(t, "tool", last.Role)
	require.Contains(t, last.Content, "hidden")
	require.Equal(t, "call-1", last.ToolCallID)
}

func TestUnit_ExecuteToolCalls_HideTools_BlocksUnqualifiedLeaf(t *testing.T) {
	hist, _, repo := runExecuteToolScope(t, &taskengine.LLMExecutionConfig{
		Tools:     []string{"local_fs"},
		HideTools: []string{"local_fs.write_file"},
	}, "write_file")

	require.Empty(t, repo.calls)
	last := hist.Messages[len(hist.Messages)-1]
	require.Equal(t, "tool", last.Role)
	require.Contains(t, last.Content, "hidden")
	require.Equal(t, "call-1", last.ToolCallID)
}

func TestUnit_ExecuteToolCalls_ExplicitEmptyToolsScope_BlocksAllRegistryTools(t *testing.T) {
	hist, _, repo := runExecuteToolScope(t, &taskengine.LLMExecutionConfig{Tools: []string{}}, "local_fs.read_file")

	require.Empty(t, repo.calls)
	last := hist.Messages[len(hist.Messages)-1]
	require.Equal(t, "tool", last.Role)
	require.Contains(t, last.Content, "not allowed")
	require.Equal(t, "call-1", last.ToolCallID)
}

func TestUnit_ExecuteToolCalls_NilToolsScope_PreservesLegacyExecution(t *testing.T) {
	_, transition, repo := runExecuteToolScope(t, &taskengine.LLMExecutionConfig{}, "local_shell.local_shell")

	require.Equal(t, taskengine.TransitionToolsExecuted, transition)
	require.Len(t, repo.calls, 1)
	require.Equal(t, "local_shell", repo.calls[0].Name)
	require.Equal(t, "local_shell", repo.calls[0].ToolName)
}

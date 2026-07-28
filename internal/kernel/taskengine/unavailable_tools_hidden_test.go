package taskengine_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/contenox/contenox/internal/kernel/taskengine"
	"github.com/contenox/contenox/internal/libtracker"
	"github.com/contenox/contenox/internal/models/llmrepo"
	libmodelprovider "github.com/contenox/contenox/internal/models/modelrepo"
	"github.com/getkin/kin-openapi/openapi3"
	"github.com/stretchr/testify/require"
)

type unavailableToolsRepo struct {
	listCalls map[string]int
}

func (r *unavailableToolsRepo) Exec(context.Context, time.Time, any, bool, *taskengine.ToolsCall) (any, taskengine.DataType, error) {
	return nil, taskengine.DataTypeAny, errors.New("Exec should not be called")
}

func (r *unavailableToolsRepo) Supports(context.Context) ([]string, error) {
	return []string{"good", "hubspot"}, nil
}

func (r *unavailableToolsRepo) GetSchemasForSupportedTools(context.Context) (map[string]*openapi3.T, error) {
	return map[string]*openapi3.T{}, nil
}

func (r *unavailableToolsRepo) GetToolsForToolsByName(_ context.Context, name string) ([]taskengine.Tool, error) {
	if r.listCalls != nil {
		r.listCalls[name]++
	}
	switch name {
	case "good":
		return []taskengine.Tool{{
			Type: "function",
			Function: taskengine.FunctionTool{
				Name:        "ping",
				Description: "Ping the available provider.",
			},
		}}, nil
	case "hubspot":
		return nil, taskengine.ToolsToolsUnavailable(name, errors.New("mcp oauth not authenticated"))
	default:
		return nil, taskengine.ErrToolsNotFound
	}
}

func TestUnit_ExecEnv_UnavailableToolsAreProbedOncePerChain(t *testing.T) {
	toolsRepo := &unavailableToolsRepo{listCalls: map[string]int{}}
	env, err := taskengine.NewEnv(context.Background(), libtracker.NoopTracker{}, &taskengine.MockTaskExecutor{
		MockOutput:          "done",
		MockTransitionValue: "done",
	}, taskengine.NewSimpleInspector(), toolsRepo)
	require.NoError(t, err)

	chain := &taskengine.TaskChainDefinition{
		ID: "dedupe-unavailable-tools",
		Tasks: []taskengine.TaskDefinition{
			{
				ID:            "one",
				Handler:       taskengine.HandleNoop,
				ExecuteConfig: &taskengine.LLMExecutionConfig{Tools: []string{"hubspot"}},
				Transition: taskengine.TaskTransition{
					Branches: []taskengine.TransitionBranch{{Operator: taskengine.OpDefault, Goto: taskengine.TermEnd}},
				},
			},
			{
				ID:            "two",
				Handler:       taskengine.HandleNoop,
				ExecuteConfig: &taskengine.LLMExecutionConfig{Tools: []string{"hubspot"}},
				Transition: taskengine.TaskTransition{
					Branches: []taskengine.TransitionBranch{{Operator: taskengine.OpDefault, Goto: taskengine.TermEnd}},
				},
			},
		},
	}

	_, _, _, err = env.ExecEnv(context.Background(), chain, "hi", taskengine.DataTypeString)
	require.NoError(t, err)
	require.Equal(t, 1, toolsRepo.listCalls["hubspot"])
}

func TestUnit_ExecEnv_UnavailableToolsAreHiddenFromModel(t *testing.T) {
	var seenMessages []libmodelprovider.Message
	var seenToolNames []string
	model := &mockModelRepo{
		chatFunc: func(_ context.Context, _ llmrepo.Request, messages []libmodelprovider.Message, opts ...libmodelprovider.ChatArgument) (libmodelprovider.ChatResult, llmrepo.Meta, error) {
			seenMessages = append([]libmodelprovider.Message(nil), messages...)
			cfg := &libmodelprovider.ChatConfig{}
			for _, opt := range opts {
				opt.Apply(cfg)
			}
			for _, tool := range cfg.Tools {
				if tool.Function != nil {
					seenToolNames = append(seenToolNames, tool.Function.Name)
				}
			}
			return libmodelprovider.ChatResult{
				Message: libmodelprovider.Message{Role: "assistant", Content: "hello"},
			}, llmrepo.Meta{ModelName: "test-model"}, nil
		},
	}

	toolsRepo := &unavailableToolsRepo{}
	exec, err := taskengine.NewExec(context.Background(), model, toolsRepo, libtracker.NoopTracker{})
	require.NoError(t, err)
	env, err := taskengine.NewEnv(context.Background(), libtracker.NoopTracker{}, exec, taskengine.NewSimpleInspector(), toolsRepo)
	require.NoError(t, err)

	chain := &taskengine.TaskChainDefinition{
		ID: "hide-unavailable-tools",
		Tasks: []taskengine.TaskDefinition{{
			ID:      "chat",
			Handler: taskengine.HandleChatCompletion,
			ExecuteConfig: &taskengine.LLMExecutionConfig{
				Model: "test-model",
				Tools: []string{"*"},
			},
			Transition: taskengine.TaskTransition{
				Branches: []taskengine.TransitionBranch{{Operator: taskengine.OpDefault, Goto: taskengine.TermEnd}},
			},
		}},
		TokenLimit: 1000,
	}

	_, _, _, err = env.ExecEnv(context.Background(), chain, "hi", taskengine.DataTypeString)
	require.NoError(t, err)

	require.Equal(t, []string{"good.ping"}, seenToolNames)
	require.NotEmpty(t, seenMessages)
	for _, msg := range seenMessages {
		require.NotContains(t, msg.Content, "hubspot")
		require.NotContains(t, msg.Content, "unavailable_tools_providers")
		require.NotContains(t, msg.Content, "mcp oauth not authenticated")
	}
}

var _ taskengine.ToolsRepo = (*unavailableToolsRepo)(nil)

package taskengine_test

import (
	"context"
	"testing"
	"time"

	"github.com/contenox/vibe/internal/hooks"
	"github.com/contenox/vibe/libtracker"
	"github.com/contenox/vibe/taskengine"
	"github.com/stretchr/testify/require"
)

type smartMockExecutor struct {
	jsonData map[string]interface{}
}

func (m *smartMockExecutor) TaskExec(ctx context.Context, startingTime time.Time, ctxLength int, chainContext *taskengine.ChainContext, currentTask *taskengine.TaskDefinition, input any, dataType taskengine.DataType) (any, taskengine.DataType, string, error) {
	if currentTask.ID == "get_data" {
		return m.jsonData, taskengine.DataTypeJSON, "success", nil
	}

	if currentTask.Handler == taskengine.HandleNoop {
		return input, dataType, "noop", nil
	}

	return input, dataType, "", nil
}

func TestUnit_SimpleEnv_ExecEnv_JSONTemplateAccess(t *testing.T) {
	jsonData := map[string]interface{}{
		"user": map[string]interface{}{
			"name": "John Doe",
			"age":  30,
			"address": map[string]interface{}{
				"city":    "New York",
				"zipcode": "10001",
			},
		},
		"repos": []interface{}{
			map[string]interface{}{
				"id":   "org/repo1",
				"name": "repo1",
				"owner": map[string]interface{}{
					"login": "org",
				},
			},
			map[string]interface{}{
				"id":   "user/repo2",
				"name": "repo2",
				"owner": map[string]interface{}{
					"login": "user",
				},
			},
		},
	}

	mockExec := &smartMockExecutor{
		jsonData: jsonData,
	}

	tracker := libtracker.NoopTracker{}
	env, err := taskengine.NewEnv(context.Background(), tracker, mockExec, taskengine.NewSimpleInspector(), hooks.NewMockHookRegistry())
	require.NoError(t, err)

	chain := &taskengine.TaskChainDefinition{
		Tasks: []taskengine.TaskDefinition{
			{
				ID:      "get_data",
				Handler: taskengine.HandleNoop,
				Transition: taskengine.TaskTransition{
					Branches: []taskengine.TransitionBranch{
						{
							Operator: taskengine.OpDefault,
							Goto:     "process_data",
						},
					},
				},
			},
			{
				ID:      "process_data",
				Handler: taskengine.HandleNoop,
				PromptTemplate: `User name: {{.get_data.user.name}}
User age: {{.get_data.user.age}}
City: {{.get_data.user.address.city}}
First repo name: {{(index .get_data.repos 0).name}}
First repo owner: {{(index (index .get_data.repos 0).owner).login}}
Second repo name: {{(index .get_data.repos 1).name}}`,
				Transition: taskengine.TaskTransition{
					Branches: []taskengine.TransitionBranch{
						{
							Operator: taskengine.OpDefault,
							Goto:     taskengine.TermEnd,
						},
					},
				},
			},
		},
	}

	result, resultType, _, err := env.ExecEnv(context.Background(), chain, nil, taskengine.DataTypeAny)
	require.NoError(t, err)
	require.Equal(t, taskengine.DataTypeString, resultType)

	expectedResult := `User name: John Doe
User age: 30
City: New York
First repo name: repo1
First repo owner: org
Second repo name: repo2`

	require.Equal(t, expectedResult, result)
}

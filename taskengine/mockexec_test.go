package taskengine_test

import (
	"context"
	"testing"
	"time"

	"github.com/contenox/vibe/taskengine"
	"github.com/stretchr/testify/require"
)

func TestUnit_TaskExec_PromptToString(t *testing.T) {
	mockExec := &taskengine.MockTaskExecutor{
		MockOutput:          "mock-result",
		MockTransitionValue: "mock-response",
		MockError:           nil,
	}

	task := &taskengine.TaskDefinition{
		Handler: taskengine.HandlePromptToString,
	}

	output, _, _, err := mockExec.TaskExec(context.Background(), time.Now(), 100, &taskengine.ChainContext{}, task, "What is 2+2?", taskengine.DataTypeString)
	require.NoError(t, err)
	require.Equal(t, "mock-result", output)
}

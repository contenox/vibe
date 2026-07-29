package taskengine_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/contenox/contenox/internal/kernel/taskengine"
	"github.com/contenox/contenox/internal/kernel/tools"
	"github.com/contenox/contenox/libtracker"
	"github.com/contenox/contenox/internal/models/llmrepo"
	libmodelprovider "github.com/contenox/contenox/internal/models/modelrepo"
	"github.com/stretchr/testify/require"
)

func TestUnit_Route_UsesShiftedHistoryPromptForLongConversation(t *testing.T) {
	var captured []libmodelprovider.Message
	routeResponse := func() (<-chan *libmodelprovider.StreamParcel, llmrepo.Meta, error) {
		ch := make(chan *libmodelprovider.StreamParcel, 2)
		ch <- &libmodelprovider.StreamParcel{Data: "coding_change"}
		ch <- &libmodelprovider.StreamParcel{Terminal: &libmodelprovider.StreamTerminal{FinishReason: "stop"}}
		close(ch)
		return ch, llmrepo.Meta{ModelName: "test-model"}, nil
	}

	repo := &mockModelRepo{
		streamFunc: func(_ context.Context, _ llmrepo.Request, messages []libmodelprovider.Message, _ ...libmodelprovider.ChatArgument) (<-chan *libmodelprovider.StreamParcel, llmrepo.Meta, error) {
			captured = append([]libmodelprovider.Message(nil), messages...)
			return routeResponse()
		},
	}

	exec, err := taskengine.NewExec(
		taskengine.WithTaskEventSink(context.Background(), &captureTaskEventSink{}),
		repo,
		tools.NewMockToolsRegistry(),
		libtracker.NoopTracker{},
	)
	require.NoError(t, err)

	hist := taskengine.ChatHistory{}
	for i := 0; i < 12; i++ {
		hist.Messages = append(hist.Messages, taskengine.Message{
			Role:    "user",
			Content: fmt.Sprintf("message-%d", i),
		})
	}

	routeTask := &taskengine.TaskDefinition{
		ID:      "classify_request",
		Handler: taskengine.HandleRoute,
		ExecuteConfig: &taskengine.LLMExecutionConfig{
			Model: "test-model",
			Shift: true,
			MaxTokens: func() *int {
				n := 1
				return &n
			}(),
		},
		Transition: taskengine.TaskTransition{
			Branches: []taskengine.TransitionBranch{
				{Operator: taskengine.OpEquals, When: "coding_change", Goto: taskengine.TermEnd},
				{Operator: taskengine.OpDefault, When: "", Goto: taskengine.TermEnd},
			},
		},
	}

	_, outputType, transition, err := exec.TaskExec(context.Background(), time.Now().UTC(), 9, &taskengine.ChainContext{}, routeTask, hist, taskengine.DataTypeChatHistory)
	require.NoError(t, err)
	require.Equal(t, taskengine.DataTypeChatHistory, outputType)
	require.Equal(t, "coding_change", transition)
	require.Len(t, captured, 2)

	prompt := captured[1].Content
	require.Contains(t, prompt, "message-5")
	require.Contains(t, prompt, "message-11")
	require.NotContains(t, prompt, "message-4")
}

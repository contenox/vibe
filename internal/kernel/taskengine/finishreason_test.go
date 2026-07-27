package taskengine_test

// A truncated model response ("length"-class finish reason) must stay visible
// all the way to the captured step: provider terminal → stream assembly →
// ChatHistory.FinishReason → CapturedStateUnit.FinishReason. Before this
// existed, the finish reason died in the step_stream_end event and a truncated
// success reached every client as a normal end of turn.

import (
	"context"
	"testing"

	"github.com/contenox/beam/internal/kernel/taskengine"
	"github.com/contenox/beam/internal/kernel/tools"
	"github.com/contenox/beam/internal/libtracker"
	"github.com/contenox/beam/internal/models/llmrepo"
	libmodelprovider "github.com/contenox/beam/internal/models/modelrepo"
	"github.com/stretchr/testify/require"
)

func TestUnit_FinishReason_TruncationSurfacesOnCapturedStep(t *testing.T) {
	sink := &captureTaskEventSink{}
	constructorCtx := taskengine.WithTaskEventSink(context.Background(), sink)

	repo := &mockModelRepo{
		streamFunc: func(ctx context.Context, req llmrepo.Request, messages []libmodelprovider.Message, opts ...libmodelprovider.ChatArgument) (<-chan *libmodelprovider.StreamParcel, llmrepo.Meta, error) {
			ch := make(chan *libmodelprovider.StreamParcel, 2)
			ch <- &libmodelprovider.StreamParcel{Data: "partial ans"}
			ch <- &libmodelprovider.StreamParcel{Terminal: &libmodelprovider.StreamTerminal{FinishReason: "length"}}
			close(ch)
			return ch, llmrepo.Meta{ModelName: "test-model", ProviderType: "test"}, nil
		},
	}

	exec, err := taskengine.NewExec(constructorCtx, repo, tools.NewMockToolsRegistry(), libtracker.NoopTracker{})
	require.NoError(t, err)
	env, err := taskengine.NewEnv(constructorCtx, libtracker.NoopTracker{}, exec, taskengine.NewSimpleInspector(), tools.NewMockToolsRegistry())
	require.NoError(t, err)

	chain := &taskengine.TaskChainDefinition{
		ID: "chain.truncated",
		Tasks: []taskengine.TaskDefinition{
			{
				ID:             "task1",
				Handler:        taskengine.HandleChatCompletion,
				PromptTemplate: "Answer at length: {{.input}}",
				ExecuteConfig:  &taskengine.LLMExecutionConfig{Model: "test-model"},
				Transition: taskengine.TaskTransition{
					Branches: []taskengine.TransitionBranch{{Operator: taskengine.OpDefault, Goto: taskengine.TermEnd}},
				},
			},
		},
	}

	result, _, units, err := env.ExecEnv(context.Background(), chain, "everything", taskengine.DataTypeString)
	require.NoError(t, err, "a truncated response is a SUCCESS carrying its finish reason, not an error")

	hist, ok := result.(taskengine.ChatHistory)
	require.True(t, ok)
	require.Equal(t, "length", hist.FinishReason, "the history carries the provider's verbatim finish reason")
	require.Contains(t, hist.Messages[len(hist.Messages)-1].Content, "partial ans", "the partial content is preserved")

	var captured string
	for i := len(units) - 1; i >= 0; i-- {
		if units[i].FinishReason != "" {
			captured = units[i].FinishReason
			break
		}
	}
	require.Equal(t, "length", captured, "the captured step exposes the truncation to stop-reason inference")
}

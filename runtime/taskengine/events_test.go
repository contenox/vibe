package taskengine_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/contenox/contenox/runtime/internal/tools"
	"github.com/contenox/contenox/runtime/internal/llmrepo"
	libmodelprovider "github.com/contenox/contenox/runtime/internal/modelrepo"
	"github.com/contenox/contenox/libbus"
	"github.com/contenox/contenox/libtracker"
	"github.com/contenox/contenox/runtime/taskengine"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type captureTaskEventSink struct {
	events []taskengine.TaskEvent
}

func (s *captureTaskEventSink) PublishTaskEvent(ctx context.Context, event taskengine.TaskEvent) error {
	s.events = append(s.events, event)
	return nil
}

func (s *captureTaskEventSink) Enabled() bool { return true }

type mockModelRepo struct {
	streamFunc func(ctx context.Context, req llmrepo.Request, messages []libmodelprovider.Message, opts ...libmodelprovider.ChatArgument) (<-chan *libmodelprovider.StreamParcel, llmrepo.Meta, error)
}

func (m *mockModelRepo) Tokenize(ctx context.Context, modelName string, prompt string) ([]int, error) {
	return []int{1}, nil
}

func (m *mockModelRepo) CountTokens(ctx context.Context, modelName string, prompt string) (int, error) {
	return 1, nil
}

func (m *mockModelRepo) PromptExecute(ctx context.Context, req llmrepo.Request, systeminstruction string, temperature float32, prompt string) (string, llmrepo.Meta, error) {
	return "", llmrepo.Meta{}, errors.New("PromptExecute should not be called")
}

func (m *mockModelRepo) Chat(ctx context.Context, req llmrepo.Request, messages []libmodelprovider.Message, opts ...libmodelprovider.ChatArgument) (libmodelprovider.ChatResult, llmrepo.Meta, error) {
	return libmodelprovider.ChatResult{}, llmrepo.Meta{}, errors.New("Chat should not be called")
}

func (m *mockModelRepo) Embed(ctx context.Context, embedReq llmrepo.EmbedRequest, prompt string) ([]float64, llmrepo.Meta, error) {
	return nil, llmrepo.Meta{}, errors.New("Embed should not be called")
}

func (m *mockModelRepo) Stream(ctx context.Context, req llmrepo.Request, messages []libmodelprovider.Message, opts ...libmodelprovider.ChatArgument) (<-chan *libmodelprovider.StreamParcel, llmrepo.Meta, error) {
	if m.streamFunc == nil {
		return nil, llmrepo.Meta{}, errors.New("streamFunc not configured")
	}
	return m.streamFunc(ctx, req, messages, opts...)
}

func TestTaskEvents_ExecEnvLifecycle(t *testing.T) {
	sink := &captureTaskEventSink{}
	constructorCtx := taskengine.WithTaskEventSink(context.Background(), sink)

	env, err := taskengine.NewEnv(constructorCtx, libtracker.NoopTracker{}, &taskengine.MockTaskExecutor{
		MockOutput:          "done",
		MockTransitionValue: "done",
	}, taskengine.NewSimpleInspector(), tools.NewMockToolsRegistry())
	require.NoError(t, err)

	chain := &taskengine.TaskChainDefinition{
		ID: "chain.lifecycle",
		Tasks: []taskengine.TaskDefinition{
			{
				ID:      "task1",
				Handler: taskengine.HandleNoop,
				Transition: taskengine.TaskTransition{
					Branches: []taskengine.TransitionBranch{{Operator: taskengine.OpEquals, When: "done", Goto: taskengine.TermEnd}},
				},
			},
		},
	}

	_, _, _, err = env.ExecEnv(context.Background(), chain, "input", taskengine.DataTypeString)
	require.NoError(t, err)
	require.Len(t, sink.events, 4)
	assert.Equal(t, []taskengine.TaskEventKind{
		taskengine.TaskEventChainStarted,
		taskengine.TaskEventStepStarted,
		taskengine.TaskEventStepCompleted,
		taskengine.TaskEventChainCompleted,
	}, []taskengine.TaskEventKind{
		sink.events[0].Kind,
		sink.events[1].Kind,
		sink.events[2].Kind,
		sink.events[3].Kind,
	})
	assert.Equal(t, "chain.lifecycle", sink.events[1].ChainID)
	assert.Equal(t, "task1", sink.events[1].TaskID)
	assert.Equal(t, "noop", sink.events[1].TaskHandler)
	assert.Equal(t, "string", sink.events[2].OutputType)
}

func TestTaskEvents_ExecEnvFailureLifecycle(t *testing.T) {
	sink := &captureTaskEventSink{}
	constructorCtx := taskengine.WithTaskEventSink(context.Background(), sink)

	env, err := taskengine.NewEnv(constructorCtx, libtracker.NoopTracker{}, &taskengine.MockTaskExecutor{
		MockError: errors.New("boom"),
	}, taskengine.NewSimpleInspector(), tools.NewMockToolsRegistry())
	require.NoError(t, err)

	chain := &taskengine.TaskChainDefinition{
		ID: "chain.failure",
		Tasks: []taskengine.TaskDefinition{
			{
				ID:      "task1",
				Handler: taskengine.HandleNoop,
			},
		},
	}

	_, _, _, err = env.ExecEnv(context.Background(), chain, "input", taskengine.DataTypeString)
	require.Error(t, err)
	require.Len(t, sink.events, 4)
	assert.Equal(t, taskengine.TaskEventStepFailed, sink.events[2].Kind)
	assert.Equal(t, taskengine.TaskEventChainFailed, sink.events[3].Kind)
	assert.Contains(t, sink.events[3].Error, "failed after")
}

func TestTaskEvents_PromptStreamingPublishesChunks(t *testing.T) {
	sink := &captureTaskEventSink{}
	constructorCtx := taskengine.WithTaskEventSink(context.Background(), sink)

	repo := &mockModelRepo{
		streamFunc: func(ctx context.Context, req llmrepo.Request, messages []libmodelprovider.Message, opts ...libmodelprovider.ChatArgument) (<-chan *libmodelprovider.StreamParcel, llmrepo.Meta, error) {
			require.Len(t, messages, 1)
			require.Equal(t, "user", messages[0].Role)
			require.Equal(t, "Say hi to world", messages[0].Content)

			ch := make(chan *libmodelprovider.StreamParcel, 3)
			ch <- &libmodelprovider.StreamParcel{Thinking: "think-1"}
			ch <- &libmodelprovider.StreamParcel{Data: "hello "}
			ch <- &libmodelprovider.StreamParcel{Data: "world"}
			close(ch)
			return ch, llmrepo.Meta{
				ModelName:    "test-model",
				ProviderType: "openai",
				BackendID:    "backend-1",
			}, nil
		},
	}

	exec, err := taskengine.NewExec(constructorCtx, repo, tools.NewMockToolsRegistry(), libtracker.NoopTracker{})
	require.NoError(t, err)
	env, err := taskengine.NewEnv(constructorCtx, libtracker.NoopTracker{}, exec, taskengine.NewSimpleInspector(), tools.NewMockToolsRegistry())
	require.NoError(t, err)

	chain := &taskengine.TaskChainDefinition{
		ID: "chain.stream",
		Tasks: []taskengine.TaskDefinition{
			{
				ID:             "task1",
				Handler:        taskengine.HandlePromptToString,
				PromptTemplate: "Say hi to {{.input}}",
				ExecuteConfig: &taskengine.LLMExecutionConfig{
					Model: "test-model",
				},
				Transition: taskengine.TaskTransition{
					Branches: []taskengine.TransitionBranch{{Operator: taskengine.OpDefault, Goto: taskengine.TermEnd}},
				},
			},
		},
	}

	result, _, _, err := env.ExecEnv(context.Background(), chain, "world", taskengine.DataTypeString)
	require.NoError(t, err)
	assert.Equal(t, "hello world", result)

	var kinds []taskengine.TaskEventKind
	var chunks []taskengine.TaskEvent
	for _, event := range sink.events {
		kinds = append(kinds, event.Kind)
		if event.Kind == taskengine.TaskEventStepChunk {
			chunks = append(chunks, event)
		}
	}
	assert.Equal(t, []taskengine.TaskEventKind{
		taskengine.TaskEventChainStarted,
		taskengine.TaskEventStepStarted,
		taskengine.TaskEventStepChunk,
		taskengine.TaskEventStepChunk,
		taskengine.TaskEventStepChunk,
		taskengine.TaskEventStepCompleted,
		taskengine.TaskEventChainCompleted,
	}, kinds)
	require.Len(t, chunks, 3)
	assert.Equal(t, "think-1", chunks[0].Thinking)
	assert.Equal(t, "hello ", chunks[1].Content)
	assert.Equal(t, "world", chunks[2].Content)
	assert.Equal(t, "test-model", chunks[2].ModelName)
}

func TestBusTaskEventSink_PublishesBroadAndRequestSubjects(t *testing.T) {
	bus := libbus.NewInMem()
	defer bus.Close()

	allCh := make(chan []byte, 1)
	reqCh := make(chan []byte, 1)
	subCtx, cancel := context.WithCancel(context.Background())
	defer cancel()

	_, err := bus.Stream(subCtx, taskengine.TaskEventSubjectAll, allCh)
	require.NoError(t, err)
	_, err = bus.Stream(subCtx, taskengine.TaskEventRequestSubject("req-1"), reqCh)
	require.NoError(t, err)

	sink := taskengine.NewBusTaskEventSink(bus)
	err = sink.PublishTaskEvent(context.Background(), taskengine.TaskEvent{
		Kind:      taskengine.TaskEventStepChunk,
		RequestID: "req-1",
		Content:   "hello",
	})
	require.NoError(t, err)

	select {
	case msg := <-allCh:
		assert.Contains(t, string(msg), "\"kind\":\"step_chunk\"")
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for broad event")
	}

	select {
	case msg := <-reqCh:
		assert.Contains(t, string(msg), "\"request_id\":\"req-1\"")
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for request event")
	}
}

func TestBusTaskEventSink_BoundsPublishTime(t *testing.T) {
	bus := libbus.NewInMem()
	defer bus.Close()

	blockedCh := make(chan []byte)
	subCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_, err := bus.Stream(subCtx, taskengine.TaskEventSubjectAll, blockedCh)
	require.NoError(t, err)

	sink := taskengine.NewBusTaskEventSink(bus)
	start := time.Now()
	err = sink.PublishTaskEvent(context.Background(), taskengine.TaskEvent{
		Kind:    taskengine.TaskEventStepChunk,
		Content: "hello",
	})
	elapsed := time.Since(start)

	require.Error(t, err)
	assert.Less(t, elapsed, 500*time.Millisecond)
}

var _ llmrepo.ModelRepo = (*mockModelRepo)(nil)
var _ taskengine.TaskEventSink = (*captureTaskEventSink)(nil)

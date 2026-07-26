package taskengine_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/contenox/beam/internal/kernel/taskengine"
	"github.com/contenox/beam/internal/kernel/tools"
	"github.com/contenox/beam/internal/libbus"
	"github.com/contenox/beam/internal/libtracker"
	"github.com/contenox/beam/internal/models/llmrepo"
	libmodelprovider "github.com/contenox/beam/internal/models/modelrepo"
	"github.com/contenox/beam/internal/models/modelrepo/openai"
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

func (s *captureTaskEventSink) Wants(taskengine.TaskEventKind) bool { return true }

type mockModelRepo struct {
	streamFunc func(ctx context.Context, req llmrepo.Request, messages []libmodelprovider.Message, opts ...libmodelprovider.ChatArgument) (<-chan *libmodelprovider.StreamParcel, llmrepo.Meta, error)
	chatFunc   func(ctx context.Context, req llmrepo.Request, messages []libmodelprovider.Message, opts ...libmodelprovider.ChatArgument) (libmodelprovider.ChatResult, llmrepo.Meta, error)
	promptFunc func(ctx context.Context, req llmrepo.Request, systeminstruction string, temperature float32, prompt string) (string, llmrepo.Meta, error)
}

func (m *mockModelRepo) Tokenize(ctx context.Context, modelName string, prompt string) ([]int, error) {
	return []int{1}, nil
}

func (m *mockModelRepo) CountTokens(ctx context.Context, modelName string, prompt string) (int, error) {
	return 1, nil
}

func (m *mockModelRepo) PromptExecute(ctx context.Context, req llmrepo.Request, systeminstruction string, temperature float32, prompt string) (string, llmrepo.Meta, error) {
	if m.promptFunc != nil {
		return m.promptFunc(ctx, req, systeminstruction, temperature, prompt)
	}
	return "", llmrepo.Meta{}, errors.New("PromptExecute should not be called")
}

func (m *mockModelRepo) Chat(ctx context.Context, req llmrepo.Request, messages []libmodelprovider.Message, opts ...libmodelprovider.ChatArgument) (libmodelprovider.ChatResult, llmrepo.Meta, error) {
	if m.chatFunc == nil {
		return libmodelprovider.ChatResult{}, llmrepo.Meta{}, errors.New("Chat should not be called")
	}
	return m.chatFunc(ctx, req, messages, opts...)
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
}

func TestTaskEvents_PrintTaskEmitsEventNotStdout(t *testing.T) {
	sink := &captureTaskEventSink{}
	cctx := taskengine.WithTaskEventSink(context.Background(), sink)

	env, err := taskengine.NewEnv(cctx, libtracker.NoopTracker{}, &taskengine.MockTaskExecutor{
		MockOutput:          "ok",
		MockTransitionValue: "ok",
	}, taskengine.NewSimpleInspector(), tools.NewMockToolsRegistry())
	require.NoError(t, err)

	chain := &taskengine.TaskChainDefinition{
		ID: "chain.print",
		Tasks: []taskengine.TaskDefinition{
			{
				ID:      "t1",
				Handler: taskengine.HandleNoop,
				Print:   "chain says hi",
				Transition: taskengine.TaskTransition{
					Branches: []taskengine.TransitionBranch{{Operator: taskengine.OpDefault, Goto: taskengine.TermEnd}},
				},
			},
		},
	}

	_, _, _, err = env.ExecEnv(libtracker.WithNewRequestID(context.Background()), chain, "in", taskengine.DataTypeString)
	require.NoError(t, err)

	var prints []taskengine.TaskEvent
	for _, e := range sink.events {
		if e.Kind == taskengine.TaskEventPrint {
			prints = append(prints, e)
		}
	}
	require.Len(t, prints, 1,
		"the chain `print` task MUST publish exactly one TaskEventPrint; a direct fmt.Println would corrupt the ACP stdio transport (stdout is the JSON-RPC channel)")
	require.Equal(t, "chain says hi", prints[0].Content)
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
	require.GreaterOrEqual(t, len(sink.events), 3)
	assert.Equal(t, taskengine.TaskEventChainFailed, sink.events[len(sink.events)-1].Kind)
	assert.Equal(t, taskengine.TaskEventStepFailed, sink.events[len(sink.events)-2].Kind)
}

func TestTaskEvents_ChatStreamingPublishesChunks(t *testing.T) {
	sink := &captureTaskEventSink{}
	constructorCtx := taskengine.WithTaskEventSink(context.Background(), sink)

	repo := &mockModelRepo{
		streamFunc: func(ctx context.Context, req llmrepo.Request, messages []libmodelprovider.Message, opts ...libmodelprovider.ChatArgument) (<-chan *libmodelprovider.StreamParcel, llmrepo.Meta, error) {
			ch := make(chan *libmodelprovider.StreamParcel, 4)
			ch <- &libmodelprovider.StreamParcel{Thinking: "think-1"}
			ch <- &libmodelprovider.StreamParcel{Data: "hello "}
			ch <- &libmodelprovider.StreamParcel{Data: "world"}
			// Raw-delta contract: a successful stream ends with a typed
			// terminal parcel.
			ch <- &libmodelprovider.StreamParcel{Terminal: &libmodelprovider.StreamTerminal{FinishReason: "stop"}}
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
				Handler:        taskengine.HandleChatCompletion,
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
	hist, ok := result.(taskengine.ChatHistory)
	require.True(t, ok)
	require.NotEmpty(t, hist.Messages)
	assert.Equal(t, "hello world", hist.Messages[len(hist.Messages)-1].Content)

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
		// Pre-check usage indicator emitted before generation begins.
		taskengine.TaskEventTokenUsage,
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

// TestTaskEvents_ChatStreamingWithToolsParsesToolCalls locks in the streaming
// path for tool-bearing chat_completion tasks, fixture-driven end to end: a
// recorded chat-completions SSE transcript (tool-call fragments split across
// chunks) is served over HTTP, decoded by the REAL openai stream adapter, and
// assembled by the engine — visible content still streams token-by-token,
// tool calls are assembled engine-side onto the assistant message (never
// leaked into the transcript as prose), and the full reply is never
// re-published as a duplicate chunk after streaming.
func TestTaskEvents_ChatStreamingWithToolsParsesToolCalls(t *testing.T) {
	sink := &captureTaskEventSink{}
	constructorCtx := taskengine.WithTaskEventSink(context.Background(), sink)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"let me \"}}]}\n\n")
		fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"check\"}}]}\n\n")
		fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call-1\",\"type\":\"function\",\"function\":{\"name\":\"get_weather\",\"arguments\":\"{\\\"city\\\":\"}}]}}]}\n\n")
		fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"\\\"Berlin\\\"}\"}}]}}]}\n\n")
		fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	provider := openai.NewOpenAIProvider("test-key", "test-model", []string{srv.URL}, libmodelprovider.CapabilityConfig{
		CanChat:   true,
		CanStream: true,
	}, srv.Client(), libtracker.NoopTracker{})

	repo := &mockModelRepo{
		streamFunc: func(ctx context.Context, req llmrepo.Request, messages []libmodelprovider.Message, opts ...libmodelprovider.ChatArgument) (<-chan *libmodelprovider.StreamParcel, llmrepo.Meta, error) {
			client, err := provider.GetStreamConnection(ctx, srv.URL)
			if err != nil {
				return nil, llmrepo.Meta{}, err
			}
			ch, err := client.Stream(ctx, messages, opts...)
			if err != nil {
				return nil, llmrepo.Meta{}, err
			}
			return ch, llmrepo.Meta{ModelName: "test-model", ProviderType: "openai", BackendID: "b1"}, nil
		},
	}

	exec, err := taskengine.NewExec(constructorCtx, repo, tools.NewMockToolsRegistry(), libtracker.NoopTracker{})
	require.NoError(t, err)
	env, err := taskengine.NewEnv(constructorCtx, libtracker.NoopTracker{}, exec, taskengine.NewSimpleInspector(), tools.NewMockToolsRegistry())
	require.NoError(t, err)

	chain := &taskengine.TaskChainDefinition{
		ID: "chain.stream.tools",
		Tasks: []taskengine.TaskDefinition{
			{
				ID:             "task1",
				Handler:        taskengine.HandleChatCompletion,
				PromptTemplate: "Weather in {{.input}}?",
				ExecuteConfig: &taskengine.LLMExecutionConfig{
					Model: "test-model",
				},
				Transition: taskengine.TaskTransition{
					Branches: []taskengine.TransitionBranch{
						{Operator: taskengine.OpEquals, When: taskengine.TransitionToolCall, Goto: taskengine.TermEnd},
						{Operator: taskengine.OpDefault, Goto: taskengine.TermEnd},
					},
				},
			},
		},
	}

	result, _, _, err := env.ExecEnv(context.Background(), chain, "Berlin", taskengine.DataTypeString)
	require.NoError(t, err)
	hist, ok := result.(taskengine.ChatHistory)
	require.True(t, ok)
	require.NotEmpty(t, hist.Messages)
	last := hist.Messages[len(hist.Messages)-1]

	// Content accumulated from streamed parcels.
	assert.Equal(t, "let me check", last.Content)
	// Tool call parsed off the terminal parcel onto the assistant message.
	require.Len(t, last.CallTools, 1)
	assert.Equal(t, "get_weather", last.CallTools[0].Function.Name)
	assert.Equal(t, `{"city":"Berlin"}`, last.CallTools[0].Function.Arguments)

	var chunks []taskengine.TaskEvent
	for _, event := range sink.events {
		if event.Kind == taskengine.TaskEventStepChunk {
			chunks = append(chunks, event)
		}
	}
	// Only the two visible-content parcels stream; the terminal tool-call parcel
	// carries no prose and must not publish a chunk.
	require.Len(t, chunks, 2, "the terminal tool-call parcel must not publish a chunk")
	assert.Equal(t, "let me ", chunks[0].Content)
	assert.Equal(t, "check", chunks[1].Content)
	// No duplicate final emission: the full reply must never appear as one extra
	// chunk after the incremental ones.
	for _, c := range chunks {
		assert.NotEqual(t, "let me check", c.Content)
	}
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

var _ llmrepo.ModelRepo = (*mockModelRepo)(nil)
var _ taskengine.TaskEventSink = (*captureTaskEventSink)(nil)

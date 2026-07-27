package taskengine_test

// Contract assertions for the engine event stream. The normative contract is
// docs/development/engine-events.md — the per-kind field matrix, ordering
// guarantees, and addressing rules asserted here. When these tests disagree
// with the document, one of the two is wrong and the slice is not done: fix
// the drift, never loosen the assertion.

import (
	"context"
	"testing"
	"time"

	"github.com/contenox/beam/internal/kernel/taskengine"
	"github.com/contenox/beam/internal/kernel/tools"
	"github.com/contenox/beam/internal/libtracker"
	"github.com/contenox/beam/internal/models/llmrepo"
	libmodelprovider "github.com/contenox/beam/internal/models/modelrepo"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestContract_StreamedRun_SequenceFieldsAndAddresses drives the
// representative streamed chat run and asserts the emitted sequence, the
// step_stream_end bracket fields, and the hierarchical address on every
// event — the matrix rows for chain_started, step_started, token_usage,
// step_chunk, step_stream_end, step_completed, chain_completed.
func TestContract_StreamedRun_SequenceFieldsAndAddresses(t *testing.T) {
	sink := &captureTaskEventSink{}
	cctx := taskengine.WithTaskEventSink(context.Background(), sink)

	repo := &mockModelRepo{
		streamFunc: func(ctx context.Context, req llmrepo.Request, messages []libmodelprovider.Message, opts ...libmodelprovider.ChatArgument) (<-chan *libmodelprovider.StreamParcel, llmrepo.Meta, error) {
			ch := make(chan *libmodelprovider.StreamParcel, 4)
			ch <- &libmodelprovider.StreamParcel{Thinking: "think-1"}
			ch <- &libmodelprovider.StreamParcel{Data: "hello "}
			ch <- &libmodelprovider.StreamParcel{Data: "world"}
			ch <- &libmodelprovider.StreamParcel{Terminal: &libmodelprovider.StreamTerminal{
				FinishReason: "stop",
				Usage:        &libmodelprovider.TokenUsage{PromptTokens: 11, CompletionTokens: 7, TotalTokens: 18},
			}}
			close(ch)
			return ch, llmrepo.Meta{ModelName: "test-model", ProviderType: "openai", BackendID: "b1"}, nil
		},
	}

	exec, err := taskengine.NewExec(cctx, repo, tools.NewMockToolsRegistry(), libtracker.NoopTracker{})
	require.NoError(t, err)
	env, err := taskengine.NewEnv(cctx, libtracker.NoopTracker{}, exec, taskengine.NewSimpleInspector(), tools.NewMockToolsRegistry())
	require.NoError(t, err)

	chain := &taskengine.TaskChainDefinition{
		ID: "chain.contract",
		Tasks: []taskengine.TaskDefinition{
			{
				ID:             "task1",
				Handler:        taskengine.HandleChatCompletion,
				PromptTemplate: "Say hi to {{.input}}",
				ExecuteConfig:  &taskengine.LLMExecutionConfig{Model: "test-model"},
				Transition: taskengine.TaskTransition{
					Branches: []taskengine.TransitionBranch{{Operator: taskengine.OpDefault, Goto: taskengine.TermEnd}},
				},
			},
		},
	}

	_, _, captured, err := env.ExecEnv(context.Background(), chain, "world", taskengine.DataTypeString)
	require.NoError(t, err)

	// Ordering guarantees: chain bracket outermost, step bracket inside it,
	// stream bracket innermost — stream_end after the LAST chunk and before
	// step_completed.
	var kinds []taskengine.TaskEventKind
	for _, ev := range sink.events {
		kinds = append(kinds, ev.Kind)
	}
	require.Equal(t, []taskengine.TaskEventKind{
		taskengine.TaskEventChainStarted,
		taskengine.TaskEventStepStarted,
		taskengine.TaskEventTokenUsage,
		taskengine.TaskEventStepChunk,
		taskengine.TaskEventStepChunk,
		taskengine.TaskEventStepChunk,
		taskengine.TaskEventStepStreamEnd,
		taskengine.TaskEventStepCompleted,
		taskengine.TaskEventChainCompleted,
	}, kinds)

	// step_stream_end field matrix: chunk count, verbatim finish reason,
	// provider usage, and model identity.
	streamEnd := sink.events[6]
	assert.Equal(t, 3, streamEnd.ChunkCount)
	assert.Equal(t, "stop", streamEnd.FinishReason)
	require.NotNil(t, streamEnd.Usage)
	assert.Equal(t, taskengine.TokenUsage{Prompt: 11, Completion: 7, Total: 18}, *streamEnd.Usage)
	assert.Equal(t, "test-model", streamEnd.ModelName)
	assert.Equal(t, "openai", streamEnd.ProviderType)
	assert.Equal(t, "b1", streamEnd.BackendID)

	// Address invariants: every event of the run names the chain; every event
	// emitted inside the task attempt names the task; chain-level events name
	// no task. No event of this run has a tool-call address.
	for i, ev := range sink.events {
		assert.Equal(t, "chain.contract", ev.Scope.Chain, "event %d (%s) must carry the chain address", i, ev.Kind)
		assert.Empty(t, ev.Scope.ToolCall, "event %d (%s) is not tool-scoped", i, ev.Kind)
		switch ev.Kind {
		case taskengine.TaskEventChainStarted, taskengine.TaskEventChainCompleted, taskengine.TaskEventChainFailed:
			assert.Empty(t, ev.Scope.Task, "chain-level event %d must not carry a task address", i)
		default:
			assert.Equal(t, "task1", ev.Scope.Task, "event %d (%s) must carry the task address", i, ev.Kind)
			// Wire compatibility: the flat fields mirror the scope.
			assert.Equal(t, ev.Scope.Chain, ev.ChainID)
			assert.Equal(t, ev.Scope.Task, ev.TaskID)
		}
	}

	// Captured state carries the same address contract.
	require.Len(t, captured, 1)
	assert.Equal(t, taskengine.EventScope{Chain: "chain.contract", Task: "task1"}, captured[0].Scope)
}

// TestContract_ToolCallEventsCarryToolCallAddress asserts the tool-call level
// of the address hierarchy: tool_call_pending and tool_call events name the
// individual invocation (Scope.ToolCall == the engine-minted call ID), on both
// the success path and the error-result path.
func TestContract_ToolCallEventsCarryToolCallAddress(t *testing.T) {
	sink := &captureTaskEventSink{}
	cctx := taskengine.WithTaskEventSink(context.Background(), sink)

	repo := &scopedExecToolsRepo{supported: []string{"local_fs"}}
	exec, err := taskengine.NewExec(cctx, &mockModelRepo{}, repo, libtracker.NoopTracker{})
	require.NoError(t, err)

	chainCtx := &taskengine.ChainContext{Tools: map[string]taskengine.ToolWithResolution{
		"local_fs.read_file": {
			Tool:      taskengine.Tool{Type: "function", Function: taskengine.FunctionTool{Name: "local_fs.read_file"}},
			ToolsName: "local_fs",
		},
	}}

	history := taskengine.ChatHistory{Messages: []taskengine.Message{{
		Role: "assistant",
		CallTools: []taskengine.ToolCall{
			{ID: "call-ok", Type: "function", Function: taskengine.FunctionCall{Name: "local_fs.read_file", Arguments: `{}`}},
			{ID: "call-missing", Type: "function", Function: taskengine.FunctionCall{Name: "no.such_tool", Arguments: `{}`}},
		},
	}}}

	taskCtx := taskengine.WithTaskEventScope(context.Background(), taskengine.TaskEventScope{
		ChainID: "chain.tools", TaskID: "exec", TaskHandler: "execute_tool_calls",
	})
	_, _, _, err = exec.TaskExec(taskCtx, time.Now().UTC(), 4000, chainCtx,
		&taskengine.TaskDefinition{ID: "exec", Handler: taskengine.HandleExecuteToolCalls},
		history, taskengine.DataTypeChatHistory)
	require.NoError(t, err)

	byKindAndCall := map[string]taskengine.TaskEvent{}
	for _, ev := range sink.events {
		byKindAndCall[string(ev.Kind)+"/"+ev.ApprovalID] = ev
		assert.Equal(t, "chain.tools", ev.Scope.Chain)
		assert.Equal(t, "exec", ev.Scope.Task)
	}

	pending, ok := byKindAndCall["tool_call_pending/call-ok"]
	require.True(t, ok, "resolvable call must emit tool_call_pending")
	assert.Equal(t, "call-ok", pending.Scope.ToolCall)

	done, ok := byKindAndCall["tool_call/call-ok"]
	require.True(t, ok, "executed call must emit tool_call")
	assert.Equal(t, "call-ok", done.Scope.ToolCall)
	assert.Empty(t, done.Error)

	failed, ok := byKindAndCall["tool_call/call-missing"]
	require.True(t, ok, "unresolvable call must emit an error tool_call")
	assert.Equal(t, "call-missing", failed.Scope.ToolCall,
		"error-result path must set the tool-call address explicitly")
	assert.NotEmpty(t, failed.Error)
}

// TestContract_MidStreamFailure_NoStreamEndBracket pins the failure ordering
// rule: a stream that dies mid-flight emits NO step_stream_end — the attempt
// is closed by step_failed (and, with no retries/on_failure, chain_failed).
func TestContract_MidStreamFailure_NoStreamEndBracket(t *testing.T) {
	sink := &captureTaskEventSink{}
	cctx := taskengine.WithTaskEventSink(context.Background(), sink)

	repo := &mockModelRepo{
		streamFunc: func(ctx context.Context, req llmrepo.Request, messages []libmodelprovider.Message, opts ...libmodelprovider.ChatArgument) (<-chan *libmodelprovider.StreamParcel, llmrepo.Meta, error) {
			ch := make(chan *libmodelprovider.StreamParcel, 2)
			ch <- &libmodelprovider.StreamParcel{Data: "partial"}
			// Stream closes without a Terminal parcel: contract violation →
			// mid-stream failure, final (no fallback re-run after publishing).
			close(ch)
			return ch, llmrepo.Meta{ModelName: "test-model", ProviderType: "openai"}, nil
		},
	}

	exec, err := taskengine.NewExec(cctx, repo, tools.NewMockToolsRegistry(), libtracker.NoopTracker{})
	require.NoError(t, err)
	env, err := taskengine.NewEnv(cctx, libtracker.NoopTracker{}, exec, taskengine.NewSimpleInspector(), tools.NewMockToolsRegistry())
	require.NoError(t, err)

	chain := &taskengine.TaskChainDefinition{
		ID: "chain.midfail",
		Tasks: []taskengine.TaskDefinition{
			{
				ID:             "task1",
				Handler:        taskengine.HandleChatCompletion,
				PromptTemplate: "{{.input}}",
				ExecuteConfig:  &taskengine.LLMExecutionConfig{Model: "test-model"},
			},
		},
	}

	_, _, _, err = env.ExecEnv(context.Background(), chain, "in", taskengine.DataTypeString)
	require.Error(t, err)

	sawChunk, sawFailed := false, false
	for _, ev := range sink.events {
		switch ev.Kind {
		case taskengine.TaskEventStepChunk:
			sawChunk = true
		case taskengine.TaskEventStepStreamEnd:
			t.Fatalf("mid-stream failure must not emit step_stream_end")
		case taskengine.TaskEventStepFailed:
			sawFailed = true
		}
	}
	assert.True(t, sawChunk, "the partial chunk streams before the failure")
	assert.True(t, sawFailed, "step_failed closes the attempt instead of a stream bracket")
	assert.Equal(t, taskengine.TaskEventChainFailed, sink.events[len(sink.events)-1].Kind)
}

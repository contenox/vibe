package taskengine_test

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

func shiftChain(shift bool, tokenLimit int64) *taskengine.TaskChainDefinition {
	return &taskengine.TaskChainDefinition{
		ID:         "shift",
		TokenLimit: tokenLimit,
		Tasks: []taskengine.TaskDefinition{
			{
				ID:            "chat",
				Handler:       taskengine.HandleChatCompletion,
				ExecuteConfig: &taskengine.LLMExecutionConfig{Model: "test-model", Shift: shift},
				Transition: taskengine.TaskTransition{
					Branches: []taskengine.TransitionBranch{{Operator: taskengine.OpDefault, Goto: taskengine.TermEnd}},
				},
			},
		},
	}
}

func userHistory(n int) taskengine.ChatHistory {
	msgs := make([]taskengine.Message, 0, n)
	for i := 0; i < n; i++ {
		msgs = append(msgs, taskengine.Message{Role: "user", Content: "m"})
	}
	return taskengine.ChatHistory{Messages: msgs}
}

func newShiftEnv(t *testing.T, sf func() (<-chan *libmodelprovider.StreamParcel, llmrepo.Meta, error), gotMsgs *int) taskengine.EnvExecutor {
	t.Helper()
	sink := &captureTaskEventSink{}
	cctx := taskengine.WithTaskEventSink(context.Background(), sink)
	repo := &mockModelRepo{
		streamFunc: func(_ context.Context, _ llmrepo.Request, messages []libmodelprovider.Message, _ ...libmodelprovider.ChatArgument) (<-chan *libmodelprovider.StreamParcel, llmrepo.Meta, error) {
			*gotMsgs = len(messages)
			return sf()
		},
	}
	exec, err := taskengine.NewExec(cctx, repo, tools.NewMockToolsRegistry(), libtracker.NoopTracker{})
	require.NoError(t, err)
	env, err := taskengine.NewEnv(cctx, libtracker.NoopTracker{}, exec, taskengine.NewSimpleInspector(), tools.NewMockToolsRegistry())
	require.NoError(t, err)
	return env
}

func newShiftEnvCapture(t *testing.T, captured *[]libmodelprovider.Message) taskengine.EnvExecutor {
	t.Helper()
	sink := &captureTaskEventSink{}
	cctx := taskengine.WithTaskEventSink(context.Background(), sink)
	repo := &mockModelRepo{
		streamFunc: func(_ context.Context, _ llmrepo.Request, messages []libmodelprovider.Message, _ ...libmodelprovider.ChatArgument) (<-chan *libmodelprovider.StreamParcel, llmrepo.Meta, error) {
			*captured = messages
			return okStream()
		},
	}
	exec, err := taskengine.NewExec(cctx, repo, tools.NewMockToolsRegistry(), libtracker.NoopTracker{})
	require.NoError(t, err)
	env, err := taskengine.NewEnv(cctx, libtracker.NoopTracker{}, exec, taskengine.NewSimpleInspector(), tools.NewMockToolsRegistry())
	require.NoError(t, err)
	return env
}

func requireNoOrphanedToolLinks(t *testing.T, msgs []libmodelprovider.Message) {
	t.Helper()
	callIDs := map[string]bool{}
	resultIDs := map[string]bool{}
	for _, m := range msgs {
		for _, tc := range m.ToolCalls {
			callIDs[tc.ID] = true
		}
		if m.Role == "tool" {
			resultIDs[m.ToolCallID] = true
		}
	}
	for _, m := range msgs {
		if m.Role == "tool" {
			require.True(t, callIDs[m.ToolCallID],
				"orphaned tool result %q (no matching assistant tool_call) — providers like Gemini/OpenAI reject this", m.ToolCallID)
		}
		for _, tc := range m.ToolCalls {
			require.True(t, resultIDs[tc.ID],
				"unanswered tool call %q (no matching tool result) — providers reject this", tc.ID)
		}
	}
}

func okStream() (<-chan *libmodelprovider.StreamParcel, llmrepo.Meta, error) {
	ch := make(chan *libmodelprovider.StreamParcel, 2)
	ch <- &libmodelprovider.StreamParcel{Data: "ok"}
	ch <- &libmodelprovider.StreamParcel{Terminal: &libmodelprovider.StreamTerminal{FinishReason: "stop"}}
	close(ch)
	return ch, llmrepo.Meta{ModelName: "test-model"}, nil
}

func TestUnit_Shift_SlidesOversizedHistoryForAnyProvider(t *testing.T) {
	gotMsgs := -1
	env := newShiftEnv(t, okStream, &gotMsgs)

	_, _, _, err := env.ExecEnv(context.Background(), shiftChain(true, 3), userHistory(8), taskengine.DataTypeChatHistory)

	require.NoError(t, err, "shift is a DSL contract: the engine MUST slide oversized history instead of erroring, regardless of provider")
	require.Equal(t, 3, gotMsgs, "history must be slid down to the token budget (8 → 3), proving the engine did the slide")
}

func TestUnit_Shift_DisabledStillErrorsOnOverflow(t *testing.T) {
	gotMsgs := -1
	env := newShiftEnv(t, okStream, &gotMsgs)

	_, _, _, err := env.ExecEnv(context.Background(), shiftChain(false, 3), userHistory(8), taskengine.DataTypeChatHistory)

	require.Error(t, err, "without shift, overflow must still hard-error (this is what triggers the recovery branch)")
	require.ErrorIs(t, err, taskengine.ErrContextLengthExceeded)
	require.Equal(t, -1, gotMsgs, "the model must never be called when the pre-flight overflow check rejects")
}

func TestUnit_Shift_DropsToolCallGroupAtomically(t *testing.T) {
	var sent []libmodelprovider.Message
	env := newShiftEnvCapture(t, &sent)

	hist := taskengine.ChatHistory{Messages: []taskengine.Message{
		{Role: "assistant", Content: "calling", CallTools: []taskengine.ToolCall{{ID: "t1", Type: "function", Function: taskengine.FunctionCall{Name: "x"}}}},
		{Role: "tool", ToolCallID: "t1", Content: "result"},
		{Role: "user", Content: "final"},
	}}
	_, _, _, err := env.ExecEnv(context.Background(), shiftChain(true, 2), hist, taskengine.DataTypeChatHistory)

	require.NoError(t, err)
	requireNoOrphanedToolLinks(t, sent)
	for _, m := range sent {
		require.NotEqual(t, "tool", m.Role, "the assistant+tool-result group must be dropped whole; a lone tool result must never survive the slide")
	}
}

func TestUnit_Shift_KeepsToolCallGroupIntactWhenItFits(t *testing.T) {
	var sent []libmodelprovider.Message
	env := newShiftEnvCapture(t, &sent)

	hist := taskengine.ChatHistory{Messages: []taskengine.Message{
		{Role: "user", Content: "old1"},
		{Role: "user", Content: "old2"},
		{Role: "assistant", Content: "calling", CallTools: []taskengine.ToolCall{{ID: "t1", Type: "function", Function: taskengine.FunctionCall{Name: "x"}}}},
		{Role: "tool", ToolCallID: "t1", Content: "result"},
	}}
	_, _, _, err := env.ExecEnv(context.Background(), shiftChain(true, 2), hist, taskengine.DataTypeChatHistory)

	require.NoError(t, err)
	requireNoOrphanedToolLinks(t, sent)
	require.Equal(t, 2, len(sent), "the newest atomic unit (assistant+result) must be kept together, older users dropped")
}

func TestUnit_Shift_PrunesPreExistingOrphanToolResult(t *testing.T) {
	var sent []libmodelprovider.Message
	env := newShiftEnvCapture(t, &sent)

	hist := taskengine.ChatHistory{Messages: []taskengine.Message{
		{Role: "tool", ToolCallID: "ghost", Content: "orphan from upstream"},
		{Role: "user", Content: "u1"},
		{Role: "user", Content: "u2"},
		{Role: "user", Content: "u3"},
	}}
	_, _, _, err := env.ExecEnv(context.Background(), shiftChain(true, 2), hist, taskengine.DataTypeChatHistory)

	require.NoError(t, err)
	requireNoOrphanedToolLinks(t, sent)
	for _, m := range sent {
		require.NotEqual(t, "tool", m.Role, "a pre-existing orphan tool result must be pruned by the slide")
	}
}

func TestUnit_Shift_UnansweredToolCallIsPairedByGuardThenKeptBySlide(t *testing.T) {
	var sent []libmodelprovider.Message
	env := newShiftEnvCapture(t, &sent)

	hist := taskengine.ChatHistory{Messages: []taskengine.Message{
		{Role: "user", Content: "u1"},
		{Role: "user", Content: "u2"},
		{Role: "user", Content: "u3"},
		{Role: "assistant", Content: "answer text", CallTools: []taskengine.ToolCall{{ID: "t1", Type: "function", Function: taskengine.FunctionCall{Name: "x"}}}},
	}}
	_, _, _, err := env.ExecEnv(context.Background(), shiftChain(true, 2), hist, taskengine.DataTypeChatHistory)

	require.NoError(t, err)
	requireNoOrphanedToolLinks(t, sent)
	var asst *libmodelprovider.Message
	var toolResult *libmodelprovider.Message
	for i := range sent {
		if sent[i].Role == "assistant" {
			asst = &sent[i]
		}
		if sent[i].Role == "tool" && sent[i].ToolCallID == "t1" {
			toolResult = &sent[i]
		}
	}
	require.NotNil(t, asst, "the assistant text must be retained")
	require.NotNil(t, toolResult, "the dangling tool call must be paired with a synthesized tool result by the HandleChatCompletion guard before shift runs, so the provider never sees an orphan")
	require.Len(t, asst.ToolCalls, 1, "the original tool_call must remain alongside its synthesized result; pairing > silent stripping")
	require.Equal(t, "answer text", asst.Content)
}

func TestUnit_Shift_IrreducibleContextStillErrorsNeverSilentEmpty(t *testing.T) {
	gotMsgs := -1
	env := newShiftEnv(t, okStream, &gotMsgs)

	hist := taskengine.ChatHistory{Messages: []taskengine.Message{
		{Role: "system", Content: "s1"},
		{Role: "system", Content: "s2"},
		{Role: "user", Content: "u"},
	}}
	_, _, _, err := env.ExecEnv(context.Background(), shiftChain(true, 1), hist, taskengine.DataTypeChatHistory)

	require.Error(t, err, "when even the irreducible context (system messages) overflows, shift must still error")
	require.ErrorIs(t, err, taskengine.ErrContextLengthExceeded)
	require.Equal(t, -1, gotMsgs, "shift must never silently send an empty/degenerate request to the model")
}

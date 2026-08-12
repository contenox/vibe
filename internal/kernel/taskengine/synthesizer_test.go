package taskengine_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/contenox/contenox/internal/kernel/taskengine"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestUnit_SynthesizeHistory_SingleChatCompletionSuccess(t *testing.T) {
	prior := []taskengine.Message{
		{ID: "m1", Role: "user", Content: "hello"},
	}
	in := taskengine.ChatHistory{Messages: prior}
	out := taskengine.ChatHistory{
		Messages: []taskengine.Message{
			prior[0],
			{ID: "m2", Role: "assistant", Content: "hi there"},
		},
	}
	units := []taskengine.CapturedStateUnit{
		{
			TaskID:      "respond",
			TaskHandler: "chat_completion",
			InputType:   taskengine.DataTypeChatHistory,
			OutputType:  taskengine.DataTypeChatHistory,
			Input:       in,
			Output:      out,
		},
	}

	got := taskengine.SynthesizeHistory(prior, units, nil)

	require.Len(t, got, 2)
	assert.Equal(t, "user", got[0].Role)
	assert.Equal(t, "assistant", got[1].Role)
	assert.Equal(t, "hi there", got[1].Content)
}

func TestUnit_SynthesizeHistory_ChatCompletionThenToolCalls(t *testing.T) {
	prior := []taskengine.Message{
		{ID: "u1", Role: "user", Content: "what's in /tmp?"},
	}
	step1Out := taskengine.ChatHistory{
		Messages: []taskengine.Message{
			prior[0],
			{
				ID: "a1", Role: "assistant", Content: "",
				CallTools: []taskengine.ToolCall{
					{ID: "c1", Type: "function", Function: taskengine.FunctionCall{Name: "list_dir", Arguments: `{"path":"/tmp"}`}},
				},
			},
		},
	}
	step2Out := taskengine.ChatHistory{
		Messages: append(append([]taskengine.Message{}, step1Out.Messages...),
			taskengine.Message{ID: "t1", Role: "tool", ToolCallID: "c1", Content: "file1\nfile2"}),
	}
	units := []taskengine.CapturedStateUnit{
		{TaskID: "ask", TaskHandler: "chat_completion", InputType: taskengine.DataTypeChatHistory, OutputType: taskengine.DataTypeChatHistory, Input: taskengine.ChatHistory{Messages: prior}, Output: step1Out},
		{TaskID: "exec", TaskHandler: "execute_tool_calls", InputType: taskengine.DataTypeChatHistory, OutputType: taskengine.DataTypeChatHistory, Input: step1Out, Output: step2Out},
	}

	got := taskengine.SynthesizeHistory(prior, units, nil)

	require.Len(t, got, 3)
	assert.Equal(t, "user", got[0].Role)
	assert.Equal(t, "assistant", got[1].Role)
	require.Len(t, got[1].CallTools, 1)
	assert.Equal(t, "list_dir", got[1].CallTools[0].Function.Name)
	assert.Equal(t, "tool", got[2].Role)
	assert.Equal(t, "c1", got[2].ToolCallID)
	assert.Equal(t, "file1\nfile2", got[2].Content)
}

func TestUnit_SynthesizeHistory_HITLDenyLandsAsToolMessage(t *testing.T) {
	const denyMsg = "User denied the operation. Please ask for clarification or try a different, less destructive approach."
	prior := []taskengine.Message{{ID: "u1", Role: "user", Content: "delete /etc/passwd"}}
	step1Out := taskengine.ChatHistory{
		Messages: []taskengine.Message{
			prior[0],
			{ID: "a1", Role: "assistant", CallTools: []taskengine.ToolCall{{ID: "c1", Function: taskengine.FunctionCall{Name: "delete_file"}}}},
		},
	}
	step2Out := taskengine.ChatHistory{
		Messages: append(append([]taskengine.Message{}, step1Out.Messages...),
			taskengine.Message{ID: "t1", Role: "tool", ToolCallID: "c1", Content: denyMsg}),
	}
	units := []taskengine.CapturedStateUnit{
		{TaskID: "ask", TaskHandler: "chat_completion", OutputType: taskengine.DataTypeChatHistory, Input: taskengine.ChatHistory{Messages: prior}, Output: step1Out},
		{TaskID: "exec", TaskHandler: "execute_tool_calls", OutputType: taskengine.DataTypeChatHistory, Input: step1Out, Output: step2Out},
	}

	got := taskengine.SynthesizeHistory(prior, units, nil)

	require.Len(t, got, 3)
	assert.Equal(t, "tool", got[2].Role)
	assert.Equal(t, denyMsg, got[2].Content)
}

func TestUnit_SynthesizeHistory_StepTimeoutAnnotation(t *testing.T) {
	prior := []taskengine.Message{{ID: "u1", Role: "user", Content: "long task"}}
	units := []taskengine.CapturedStateUnit{
		{
			TaskID:      "respond",
			TaskHandler: "chat_completion",
			InputType:   taskengine.DataTypeChatHistory,
			OutputType:  taskengine.DataTypeAny,
			Input:       taskengine.ChatHistory{Messages: prior},
			Error:       taskengine.ErrorResponse{Error: "context deadline exceeded"},
			TimedOut:    true,
		},
	}

	got := taskengine.SynthesizeHistory(prior, units, errors.New("task respond: context deadline exceeded"))

	require.Len(t, got, 2)
	assert.Equal(t, "assistant", got[1].Role)
	assert.Contains(t, got[1].Content, "respond")
	assert.Contains(t, got[1].Content, "timed out")
}

func TestUnit_SynthesizeHistory_StepCancelledAnnotation(t *testing.T) {
	prior := []taskengine.Message{{ID: "u1", Role: "user", Content: "x"}}
	units := []taskengine.CapturedStateUnit{
		{
			TaskID:      "respond",
			TaskHandler: "chat_completion",
			Input:       taskengine.ChatHistory{Messages: prior},
			Error:       taskengine.ErrorResponse{Error: "context canceled"},
			Cancelled:   true,
		},
	}

	got := taskengine.SynthesizeHistory(prior, units, nil)

	require.Len(t, got, 2)
	assert.Contains(t, got[1].Content, "cancelled")
}

func TestUnit_SynthesizeHistory_ChainErrBetweenSteps(t *testing.T) {
	prior := []taskengine.Message{{ID: "u1", Role: "user", Content: "x"}}
	out := taskengine.ChatHistory{
		Messages: []taskengine.Message{
			prior[0],
			{ID: "a1", Role: "assistant", Content: "ok"},
		},
	}
	units := []taskengine.CapturedStateUnit{
		{TaskID: "respond", TaskHandler: "chat_completion", OutputType: taskengine.DataTypeChatHistory, Input: taskengine.ChatHistory{Messages: prior}, Output: out},
	}

	got := taskengine.SynthesizeHistory(prior, units, errors.New("transition target not found: missing"))

	require.Len(t, got, 3)
	assert.Equal(t, "assistant", got[2].Role)
	assert.Contains(t, got[2].Content, "chain failed")
	assert.Contains(t, got[2].Content, "transition target")
}

func TestUnit_SynthesizeHistory_EmptyUnitsWithChainErr(t *testing.T) {
	got := taskengine.SynthesizeHistory(nil, nil, errors.New("boom"))

	require.Len(t, got, 1)
	assert.Equal(t, "assistant", got[0].Role)
	assert.Contains(t, got[0].Content, "boom")
}

func TestUnit_SynthesizeHistory_EmptyEverything(t *testing.T) {
	got := taskengine.SynthesizeHistory(nil, nil, nil)
	assert.Empty(t, got)
}

func TestUnit_SynthesizeHistory_DedupeByID(t *testing.T) {
	prior := []taskengine.Message{
		{ID: "u1", Role: "user", Content: "hi"},
		{ID: "a1", Role: "assistant", Content: "hello"},
	}
	in := taskengine.ChatHistory{Messages: prior[:1]}
	out := taskengine.ChatHistory{Messages: prior}
	units := []taskengine.CapturedStateUnit{
		{TaskID: "x", TaskHandler: "chat_completion", OutputType: taskengine.DataTypeChatHistory, Input: in, Output: out},
	}

	got := taskengine.SynthesizeHistory(prior, units, nil)

	require.Len(t, got, 2)
	ids := make([]string, 0, len(got))
	for _, m := range got {
		ids = append(ids, m.ID)
	}
	assert.Equal(t, []string{"u1", "a1"}, ids)
}

func TestUnit_SynthesizeHistory_StableAcrossRepeatedRuns(t *testing.T) {
	prior := []taskengine.Message{{ID: "u1", Role: "user", Content: "hi"}}
	out := taskengine.ChatHistory{
		Messages: []taskengine.Message{prior[0], {ID: "a1", Role: "assistant", Content: "hello"}},
	}
	units := []taskengine.CapturedStateUnit{
		{TaskID: "x", TaskHandler: "chat_completion", OutputType: taskengine.DataTypeChatHistory, Input: taskengine.ChatHistory{Messages: prior}, Output: out},
	}

	first := taskengine.SynthesizeHistory(prior, units, nil)
	second := taskengine.SynthesizeHistory(prior, units, nil)

	require.Len(t, first, 2)
	require.Len(t, second, 2)
	assert.Equal(t, first[0].ID, second[0].ID)
	assert.Equal(t, first[1].ID, second[1].ID)
	assert.Equal(t, first[1].Content, second[1].Content)
}

func TestUnit_SynthesizeHistory_NonChatHistoryOutputIsIgnored(t *testing.T) {
	prior := []taskengine.Message{{ID: "u1", Role: "user", Content: "hi"}}
	units := []taskengine.CapturedStateUnit{
		{TaskID: "route", TaskHandler: "prompt_to_string", OutputType: taskengine.DataTypeString, Input: "hi", Output: "valid", Transition: "valid"},
	}

	got := taskengine.SynthesizeHistory(prior, units, nil)

	require.Len(t, got, 1)
	assert.Equal(t, "u1", got[0].ID)
}

func TestUnit_SynthesizeHistory_ErrorAfterPartialOutput(t *testing.T) {
	prior := []taskengine.Message{{ID: "u1", Role: "user", Content: "x"}}
	out := taskengine.ChatHistory{
		Messages: []taskengine.Message{
			prior[0],
			{ID: "a1", Role: "assistant", Content: "partial"},
		},
	}
	units := []taskengine.CapturedStateUnit{
		{
			TaskID:      "respond",
			TaskHandler: "chat_completion",
			OutputType:  taskengine.DataTypeChatHistory,
			Input:       taskengine.ChatHistory{Messages: prior},
			Output:      out,
			Error:       taskengine.ErrorResponse{Error: "stream interrupted"},
		},
	}

	got := taskengine.SynthesizeHistory(prior, units, nil)

	require.Len(t, got, 3)
	assert.Equal(t, "partial", got[1].Content)
	assert.Contains(t, strings.ToLower(got[2].Content), "failed")
	assert.Contains(t, got[2].Content, "stream interrupted")
}

func TestUnit_SynthesizeHistory_ErrorDropsEmptyAssistantShell(t *testing.T) {
	prior := []taskengine.Message{{ID: "u1", Role: "user", Content: "x"}}
	out := taskengine.ChatHistory{
		Messages: []taskengine.Message{
			prior[0],
			{ID: "empty", Role: "assistant"},
		},
	}
	units := []taskengine.CapturedStateUnit{
		{
			TaskID:      "respond",
			TaskHandler: "chat_completion",
			OutputType:  taskengine.DataTypeChatHistory,
			Input:       taskengine.ChatHistory{Messages: prior},
			Output:      out,
			Error:       taskengine.ErrorResponse{Error: "llamacppshim: common chat parse: The model produced output that does not match the expected peg-native format"},
		},
	}

	got := taskengine.SynthesizeHistory(prior, units, nil)

	require.Len(t, got, 2)
	assert.Equal(t, "user", got[0].Role)
	assert.Equal(t, "assistant", got[1].Role)
	assert.Contains(t, got[1].Content, "peg-native format")
}

// TestUnit_SynthesizeHistory_SystemPrependDoesNotReemitOrPersist guards against index-based diffing re-emitting messages or leaking the injected system instruction.
func TestUnit_SynthesizeHistory_SystemPrependDoesNotReemitOrPersist(t *testing.T) {
	prior := []taskengine.Message{
		{ID: "u1", Role: "user", Content: "read the file"},
		{ID: "a0", Role: "assistant", CallTools: []taskengine.ToolCall{{ID: "c0", Function: taskengine.FunctionCall{Name: "read_file"}}}},
		{ID: "t0", Role: "tool", ToolCallID: "c0", Content: "contents"},
	}
	out := taskengine.ChatHistory{
		Messages: append(
			[]taskengine.Message{{ID: "sys1", Role: "system", Content: "task instruction"}},
			append(append([]taskengine.Message{}, prior...),
				taskengine.Message{ID: "a1", Role: "assistant", Content: "the file says: contents"})...),
	}
	units := []taskengine.CapturedStateUnit{
		{TaskID: "chat", TaskHandler: "chat_completion", OutputType: taskengine.DataTypeChatHistory, Input: taskengine.ChatHistory{Messages: prior}, Output: out},
	}

	got := taskengine.SynthesizeHistory(prior, units, nil)

	require.Len(t, got, 4)
	ids := make([]string, 0, len(got))
	for _, m := range got {
		require.NotEqual(t, "system", m.Role)
		ids = append(ids, m.ID)
	}
	assert.Equal(t, []string{"u1", "a0", "t0", "a1"}, ids)
}

// TestUnit_SynthesizeHistory_InterruptedToolCallGetsStubResult guards against persisting an unanswered tool call when the chain dies mid-execution.
func TestUnit_SynthesizeHistory_InterruptedToolCallGetsStubResult(t *testing.T) {
	prior := []taskengine.Message{{ID: "u1", Role: "user", Content: "list files"}}
	chatOut := taskengine.ChatHistory{
		Messages: []taskengine.Message{
			prior[0],
			{ID: "a1", Role: "assistant", CallTools: []taskengine.ToolCall{{ID: "c1", Function: taskengine.FunctionCall{Name: "list_dir"}}}},
		},
	}
	units := []taskengine.CapturedStateUnit{
		{TaskID: "chat", TaskHandler: "chat_completion", OutputType: taskengine.DataTypeChatHistory, Input: taskengine.ChatHistory{Messages: prior}, Output: chatOut},
	}

	got := taskengine.SynthesizeHistory(prior, units, errors.New("stream interrupted"))

	require.Len(t, got, 4)
	assert.Equal(t, "a1", got[1].ID)
	assert.Equal(t, "tool", got[2].Role)
	assert.Equal(t, "c1", got[2].ToolCallID)
	assert.Contains(t, got[2].Content, "interrupted")
	assert.Contains(t, got[3].Content, "chain failed")
}

// TestUnit_SynthesizeHistory_OrphanToolResultDropped guards against carrying forward tool results whose call is gone.
func TestUnit_SynthesizeHistory_OrphanToolResultDropped(t *testing.T) {
	prior := []taskengine.Message{
		{ID: "u1", Role: "user", Content: "hi"},
		{ID: "t-ghost", Role: "tool", ToolCallID: "ghost", Content: "orphan"},
		{ID: "a1", Role: "assistant", Content: "hello"},
	}

	got := taskengine.SynthesizeHistory(prior, nil, nil)

	require.Len(t, got, 2)
	assert.Equal(t, "u1", got[0].ID)
	assert.Equal(t, "a1", got[1].ID)
}

// TestUnit_SynthesizeHistory_DuplicateToolResultsDropped guards against duplicate tool results for one call ID; only the first must survive.
func TestUnit_SynthesizeHistory_DuplicateToolResultsDropped(t *testing.T) {
	prior := []taskengine.Message{
		{ID: "u1", Role: "user", Content: "read it"},
		{ID: "a1", Role: "assistant", CallTools: []taskengine.ToolCall{{ID: "c1", Function: taskengine.FunctionCall{Name: "read_file"}}}},
		{ID: "t1", Role: "tool", ToolCallID: "c1", Content: "contents"},
		{ID: "t1-dup", Role: "tool", ToolCallID: "c1", Content: "contents"},
	}

	got := taskengine.SynthesizeHistory(prior, nil, nil)

	require.Len(t, got, 3)
	assert.Equal(t, "t1", got[2].ID)
}

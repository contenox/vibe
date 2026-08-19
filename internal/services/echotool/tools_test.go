package echotool

import (
	"context"
	"testing"
	"time"

	"github.com/contenox/contenox/internal/kernel/taskengine"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func exec(t *testing.T, input any, call *taskengine.ToolsCall) (any, taskengine.DataType, error) {
	t.Helper()
	return NewTools().Exec(context.Background(), time.Now(), input, false, call)
}

func echoCall() *taskengine.ToolsCall {
	return &taskengine.ToolsCall{Name: ToolsProviderName, ToolName: ToolEcho}
}

func TestUnit_Echo_ReturnsItsInput(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		input any
		want  any
		dt    taskengine.DataType
	}{
		{"a string is echoed verbatim", "hello", "hello", taskengine.DataTypeString},
		{"an empty string stays empty", "", "", taskengine.DataTypeString},
		{"the input argument is echoed", map[string]any{"input": "hello"}, "hello", taskengine.DataTypeString},
		{"a non-string argument is rendered", map[string]any{"input": 42}, "42", taskengine.DataTypeString},
		{"a boolean argument is rendered", map[string]any{"input": true}, "true", taskengine.DataTypeString},
		{"no input argument is not an error", map[string]any{}, nothingToEcho, taskengine.DataTypeString},
		{"no input at all is not an error", nil, nothingToEcho, taskengine.DataTypeString},
		{"an unhandled input type is rendered", 7, "7", taskengine.DataTypeString},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			out, dt, err := exec(t, tc.input, echoCall())
			require.NoError(t, err)
			assert.Equal(t, tc.want, out)
			assert.Equal(t, tc.dt, dt)
		})
	}
}

// A declarative `tools` task carries its arguments on the call, not in the
// chain input, so both paths must reach the same argument map.
func TestUnit_Echo_ArgumentsMayComeFromTheCall(t *testing.T) {
	t.Parallel()

	call := echoCall()
	call.Args = map[string]string{"input": "from the call"}

	for _, input := range []any{nil, map[string]any{}} {
		out, _, err := exec(t, input, call)
		require.NoError(t, err)
		assert.Equal(t, "from the call", out)
	}

	// The chain input wins when it carries arguments of its own.
	out, _, err := exec(t, map[string]any{"input": "from the input"}, call)
	require.NoError(t, err)
	assert.Equal(t, "from the input", out)
}

// Argument names are strict: a misspelled one is a refusal that names the fix,
// not a silent "nothing to echo" the model has to diagnose.
func TestUnit_Echo_RejectsUnknownArguments(t *testing.T) {
	t.Parallel()

	_, _, err := exec(t, map[string]any{"message": "hi"}, echoCall())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown argument(s): message")
	assert.Contains(t, err.Error(), "allowed: input")
	assert.Contains(t, err.Error(), severityRecoverable)
}

func TestUnit_Echo_RefusesAnUnknownTool(t *testing.T) {
	t.Parallel()

	_, _, err := exec(t, "hi", &taskengine.ToolsCall{Name: ToolsProviderName, ToolName: "shout"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), `unknown tool "shout"`)
	assert.Contains(t, err.Error(), ToolEcho)
}

func TestUnit_Echo_RefusesACallWithNoToolsCall(t *testing.T) {
	t.Parallel()

	_, _, err := exec(t, "hi", nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "tools required")
}

// This toolset has exactly one tool, so a call that names only the provider is
// unambiguous and resolves to it.
func TestUnit_Echo_ProviderNameAloneResolves(t *testing.T) {
	t.Parallel()

	out, _, err := exec(t, "hi", &taskengine.ToolsCall{Name: ToolsProviderName})
	require.NoError(t, err)
	assert.Equal(t, "hi", out)
}

func TestUnit_Echo_ChatHistoryGainsOneAssistantTurn(t *testing.T) {
	t.Parallel()

	history := taskengine.ChatHistory{Messages: []taskengine.Message{
		{Role: "user", Content: "first"},
		{Role: "assistant", Content: "answer"},
		{Role: "user", Content: "second"},
	}}
	out, dt, err := exec(t, history, echoCall())
	require.NoError(t, err)
	require.Equal(t, taskengine.DataTypeChatHistory, dt)

	got := out.(taskengine.ChatHistory)
	require.Len(t, got.Messages, 4)
	assert.Equal(t, "assistant", got.Messages[3].Role)
	assert.Equal(t, "Echo: second", got.Messages[3].Content)
	assert.False(t, got.Messages[3].Timestamp.IsZero())
}

func TestUnit_Echo_ChatHistoryWithoutAUserMessage(t *testing.T) {
	t.Parallel()

	history := taskengine.ChatHistory{Messages: []taskengine.Message{
		{Role: "system", Content: "be brief"},
		{Role: "user", Content: ""},
	}}
	out, _, err := exec(t, history, echoCall())
	require.NoError(t, err)
	assert.Equal(t, "Echo: "+nothingToEcho, out.(taskengine.ChatHistory).Messages[2].Content)
}

// The engine hands the same history to every step, so appending in place would
// write the echo into the caller's own slice.
func TestUnit_Echo_ChatHistoryIsNotMutatedInPlace(t *testing.T) {
	t.Parallel()

	messages := make([]taskengine.Message, 1, 8)
	messages[0] = taskengine.Message{Role: "user", Content: "only"}
	history := taskengine.ChatHistory{Messages: messages}

	_, _, err := exec(t, history, echoCall())
	require.NoError(t, err)
	assert.Len(t, history.Messages, 1)
	assert.Empty(t, messages[:2][1].Content, "the echo was written into the caller's backing array")
}

// A pointer history answers with the value type, or the DataTypeChatHistory
// consumers downstream see a shape they do not assert for.
func TestUnit_Echo_PointerChatHistoryAnswersWithTheValueType(t *testing.T) {
	t.Parallel()

	history := &taskengine.ChatHistory{Messages: []taskengine.Message{{Role: "user", Content: "ping"}}}
	out, dt, err := exec(t, history, echoCall())
	require.NoError(t, err)
	require.Equal(t, taskengine.DataTypeChatHistory, dt)
	got, ok := out.(taskengine.ChatHistory)
	require.True(t, ok, "a pointer history answered with %T", out)
	assert.Equal(t, "Echo: ping", got.Messages[1].Content)
	assert.Len(t, history.Messages, 1)

	// A nil pointer is a call with nothing in it, not a panic.
	out, dt, err = exec(t, (*taskengine.ChatHistory)(nil), echoCall())
	require.NoError(t, err)
	assert.Equal(t, taskengine.DataTypeString, dt)
	assert.Equal(t, nothingToEcho, out)
}

// The result is what the model reads, so it is a string or a history and never
// a Go struct dump.
func TestUnit_Echo_ResultIsAlwaysRenderable(t *testing.T) {
	t.Parallel()

	for _, input := range []any{"s", map[string]any{"input": "s"}, nil, 1.5, []string{"a"}, struct{ A int }{1}} {
		out, dt, err := exec(t, input, echoCall())
		require.NoError(t, err)
		require.Equal(t, taskengine.DataTypeString, dt)
		_, ok := out.(string)
		assert.Truef(t, ok, "input %T answered with %T", input, out)
	}
}

func TestUnit_Echo_DescriptorNamesItselfAFixture(t *testing.T) {
	t.Parallel()

	all, err := NewTools().GetToolsForToolsByName(context.Background(), ToolsProviderName)
	require.NoError(t, err)
	desc := all[0].Function.Description
	for _, want := range []string{"fixture", "NOT a capability", "reads no file"} {
		assert.Containsf(t, desc, want, "the description does not state that the tool does nothing:\n%s", desc)
	}
}

// The published contract covers every declared tool, or a declared toolset ships
// a definition nothing describes.
func TestUnit_Echo_PublishedContractCoversTheDescriptor(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	repo := NewTools()
	docs, err := repo.GetSchemasForSupportedTools(ctx)
	require.NoError(t, err)
	doc := docs[ToolsProviderName]
	require.NotNil(t, doc)
	require.Contains(t, doc.Components.Schemas, "EchoRequest")
	require.Contains(t, doc.Components.Schemas, "EchoResponse")
	assert.Len(t, doc.Components.Schemas["EchoResponse"].Value.OneOf, 2,
		"the response schema no longer covers both the string and the chat-history form")
	assert.Equal(t, []string{"input"}, doc.Components.Schemas["EchoRequest"].Value.Required)
}

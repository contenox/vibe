package taskengine

import (
	"testing"

	"github.com/stretchr/testify/require"
)

var allHandlers = []TaskHandler{
	HandleRaiseError,
	HandleRoute,
	HandleChatCompletion,
	HandleExecuteToolCalls,
	HandleNoop,
	HandleTools,
}

// TestUnit_IsAssistantProseHandler_CoversEveryHandler pins which handlers' streamed chunks are user-visible assistant narration (only chat_completion).
func TestUnit_IsAssistantProseHandler_CoversEveryHandler(t *testing.T) {
	prose := map[TaskHandler]bool{
		HandleChatCompletion: true,
	}
	for _, h := range allHandlers {
		require.Equal(t, prose[h], IsAssistantProseHandler(h.String()),
			"handler %q: decide explicitly whether its streamed chunks are assistant prose", h)
	}

	require.False(t, IsAssistantProseHandler("a_handler_added_tomorrow"))
	require.False(t, IsAssistantProseHandler(""))
}

// TestUnit_IsToolBearingHandler_CoversEveryHandler pins which handlers already report their own work through tool-call events, suppressing the generic step-lifecycle card for them.
func TestUnit_IsToolBearingHandler_CoversEveryHandler(t *testing.T) {
	toolBearing := map[TaskHandler]bool{
		HandleChatCompletion:   true,
		HandleExecuteToolCalls: true,
		HandleTools:            true,
		HandleRoute:            true,
	}
	for _, h := range allHandlers {
		require.Equal(t, toolBearing[h], IsToolBearingHandler(h.String()),
			"handler %q: decide explicitly whether it renders its own tool calls", h)
	}

	require.False(t, IsToolBearingHandler("a_handler_added_tomorrow"))
	require.False(t, IsToolBearingHandler(""))
}

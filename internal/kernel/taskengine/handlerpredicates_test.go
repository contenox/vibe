package taskengine

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// allHandlers is the complete handler vocabulary. Both predicate tests below
// iterate it rather than listing cases inline, so adding a handler to tasktype.go
// without deciding what the event translators should do with it fails HERE — in
// the package that owns the vocabulary — instead of silently defaulting on two
// surfaces (runtime/acpsvc and runtime/vscodeagent) that each used to carry their
// own copy of these judgements.
var allHandlers = []TaskHandler{
	HandleRaiseError,
	HandleRoute,
	HandleChatCompletion,
	HandleExecuteToolCalls,
	HandleNoop,
	HandleTools,
}

// TestUnit_IsAssistantProseHandler_CoversEveryHandler pins which handlers'
// streamed chunks are user-visible assistant narration.
//
// Only chat_completion is. Empirically only route and chat_completion can emit a
// TaskEventStepChunk at all — SimpleExec.publishStepChunk is reached from exactly
// three places, the streaming branch of Prompt (which only the route handler
// calls) and the streaming + non-streaming branches of executeLLM (which only the
// chat_completion handler calls) — so a "drop route" blocklist and a "forward only
// chat_completion" allowlist agree today. The allowlist is nevertheless the
// spelling this predicate implements: an unknown handler must be silent, not
// forwarded.
func TestUnit_IsAssistantProseHandler_CoversEveryHandler(t *testing.T) {
	prose := map[TaskHandler]bool{
		HandleChatCompletion: true,
	}
	for _, h := range allHandlers {
		require.Equal(t, prose[h], IsAssistantProseHandler(h.String()),
			"handler %q: decide explicitly whether its streamed chunks are assistant prose", h)
	}

	// A handler nobody has taught this predicate about is NOT prose. This is the
	// whole reason the allowlist spelling won: the blocklist variant would have
	// leaked a new handler's streamed internals into every transcript.
	require.False(t, IsAssistantProseHandler("a_handler_added_tomorrow"))
	require.False(t, IsAssistantProseHandler(""))
}

// TestUnit_IsToolBearingHandler_CoversEveryHandler pins which handlers already
// report their own work through tool-call events, so the generic step-lifecycle
// card must be suppressed for them. Previously duplicated verbatim in acpsvc and
// vscodeagent with only the acpsvc copy tested.
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

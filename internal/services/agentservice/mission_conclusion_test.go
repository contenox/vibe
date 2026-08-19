package agentservice

import (
	"testing"

	"github.com/contenox/contenox/internal/kernel/taskengine"
)

func unitWithMessages(msgs ...taskengine.Message) taskengine.CapturedStateUnit {
	return taskengine.CapturedStateUnit{
		OutputType: taskengine.DataTypeChatHistory,
		Output:     taskengine.ChatHistory{Messages: msgs},
	}
}

func finishCall(id string) taskengine.Message {
	m := taskengine.Message{Role: "assistant"}
	m.CallTools = []taskengine.ToolCall{{ID: id, Function: taskengine.FunctionCall{
		Name:      "mission.mission_finish",
		Arguments: `{"status":"landed"}`,
	}}}
	return m
}

// A teardown cancellation after a landed mission_finish is a clean stop; the
// suppression must fire only when the finish call was answered successfully.
func TestUnit_RunConcludedItsMission(t *testing.T) {
	answered := []taskengine.CapturedStateUnit{unitWithMessages(
		finishCall("call-1"),
		taskengine.Message{Role: "tool", ToolCallID: "call-1", Content: "mission finished as landed"},
	)}
	if !runConcludedItsMission(answered) {
		t.Fatal("a finish call answered with the success result concludes the run")
	}

	unanswered := []taskengine.CapturedStateUnit{unitWithMessages(finishCall("call-2"))}
	if runConcludedItsMission(unanswered) {
		t.Fatal("a finish call with no result must not conclude the run")
	}

	refused := []taskengine.CapturedStateUnit{unitWithMessages(
		finishCall("call-3"),
		taskengine.Message{Role: "tool", ToolCallID: "call-3", Content: "cannot finish mission: a terminal status is required"},
	)}
	if runConcludedItsMission(refused) {
		t.Fatal("a refused finish must not conclude the run")
	}

	if runConcludedItsMission(nil) {
		t.Fatal("no units, no conclusion")
	}
}

package taskengine

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

// TestUnit_TransitionTokens_FrozenValues pins the transition-eval token strings.
// These are a de-facto public API: chains branch on them via TransitionBranch.When.
// Changing a value here silently breaks every chain that branches on it.
func TestUnit_TransitionTokens_FrozenValues(t *testing.T) {
	require.Equal(t, "tool_call", TransitionToolCall, "snake_case, aligned with the tool_call task-event kind (was the hyphenated tool-call pre-1.0)")
	require.Equal(t, "executed", TransitionExecuted)
	require.Equal(t, "noop", TransitionNoop)
	require.Equal(t, "no_calls_found", TransitionNoCallsFound)
	require.Equal(t, "tools_executed", TransitionToolsExecuted)
	require.Equal(t, "failed", TransitionFailed)
}

// TestUnit_DataType_RoundTrips guards C1/C2: every DataType must survive a
// JSON and YAML round-trip via its string name, including DataTypeNil and
// DataTypeAny (which previously diverged / failed).
func TestUnit_DataType_RoundTrips(t *testing.T) {
	for _, dt := range []DataType{DataTypeAny, DataTypeString, DataTypeInt, DataTypeJSON, DataTypeChatHistory, DataTypeNil} {
		jb, err := json.Marshal(dt)
		require.NoError(t, err)
		var jOut DataType
		require.NoError(t, json.Unmarshal(jb, &jOut), "json round-trip for %s", dt.String())
		require.Equal(t, dt, jOut, "json value drifted for %s (bytes=%s)", dt.String(), jb)

		yb, err := yaml.Marshal(dt)
		require.NoError(t, err)
		var yOut DataType
		require.NoError(t, yaml.Unmarshal(yb, &yOut), "yaml round-trip for %s", dt.String())
		require.Equal(t, dt, yOut, "yaml value drifted for %s (bytes=%s)", dt.String(), yb)
	}
}

// TestUnit_EmptyBranches_CleanEnd guards E2: a leaf task (no branches) ends the
// chain cleanly instead of erroring with "no matching transition found".
func TestUnit_EmptyBranches_CleanEnd(t *testing.T) {
	next, branch, err := SimpleEnv{}.evaluateTransitions(context.TODO(), "leaf", TaskTransition{Branches: nil}, "anything", nil)
	require.NoError(t, err, "a task with no branches must end the chain, not error")
	require.Equal(t, "", next, "empty next task id signals chain end")
	require.Nil(t, branch)
}

// TestUnit_ValidateChain_Tightening guards A1–A5: malformed chains that used to
// pass validation and fail opaquely at runtime are now rejected up front.
func TestUnit_ValidateChain_Tightening(t *testing.T) {
	end := []TransitionBranch{{Operator: OpDefault, When: "", Goto: TermEnd}}
	ok := func(id string) TaskDefinition {
		return TaskDefinition{ID: id, Handler: HandleNoop, Transition: TaskTransition{Branches: end}}
	}

	require.NoError(t, validateChain([]TaskDefinition{ok("a")}), "a minimal valid chain must pass")

	cases := map[string][]TaskDefinition{
		"duplicate id":     {ok("a"), ok("a")},
		"unknown handler":  {{ID: "a", Handler: "chat_completon", Transition: TaskTransition{Branches: end}}},
		"empty operator":   {{ID: "a", Handler: HandleNoop, Transition: TaskTransition{Branches: []TransitionBranch{{When: "x", Goto: TermEnd}}}}},
		"unknown operator": {{ID: "a", Handler: HandleNoop, Transition: TaskTransition{Branches: []TransitionBranch{{Operator: "eq", When: "x", Goto: TermEnd}}}}},
		"dangling goto":    {{ID: "a", Handler: HandleNoop, Transition: TaskTransition{Branches: []TransitionBranch{{Operator: OpDefault, Goto: "ghost"}}}}},
		"dangling onfail":  {{ID: "a", Handler: HandleNoop, Transition: TaskTransition{OnFailure: "ghost", Branches: end}}},
		"tools no block":   {{ID: "a", Handler: HandleTools, Transition: TaskTransition{Branches: end}}},
	}
	for name, tasks := range cases {
		t.Run(name, func(t *testing.T) {
			require.Error(t, validateChain(tasks), "%s must be rejected at validation", name)
		})
	}
}

// TestUnit_TemperatureValue guards E1's nullability: nil means "do not send a
// temperature" (provider default), a set value is forwarded.
func TestUnit_TemperatureValue(t *testing.T) {
	_, ok := temperatureValue(nil)
	require.False(t, ok, "nil temperature must not be sent so the provider default applies")

	v := float32(0.0)
	got, ok := temperatureValue(&v)
	require.True(t, ok, "an explicit 0.0 must be sent, not treated as unset")
	require.Equal(t, float32(0.0), got)
}

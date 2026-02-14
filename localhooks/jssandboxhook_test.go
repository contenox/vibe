// jssandboxhook_test.go
package localhooks

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/contenox/vibe/jseval"
	"github.com/contenox/vibe/libtracker"
	"github.com/contenox/vibe/taskengine"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// helper: construct a JSSandboxHook with a real jseval.Env but no builtins wired.
func newTestJSSandboxHook(t *testing.T) *JSSandboxHook {
	t.Helper()

	tracker := libtracker.NewLogActivityTracker(slog.Default())
	env := jseval.NewEnv(tracker, jseval.BuiltinHandlers{})

	hookRepo := NewJSSandboxHook(env, tracker)
	jsHook, ok := hookRepo.(*JSSandboxHook)
	require.True(t, ok, "expected *JSSandboxHook from NewJSSandboxHook")

	return jsHook
}

func TestUnit_JSSandboxHook_Supports(t *testing.T) {
	ctx := context.Background()
	h := newTestJSSandboxHook(t)

	names, err := h.Supports(ctx)
	require.NoError(t, err)
	require.Len(t, names, 1)
	assert.Equal(t, jsSandboxHookName, names[0])
}

func TestUnit_JSSandboxHook_GetSchemasForSupportedHooks(t *testing.T) {
	ctx := context.Background()
	h := newTestJSSandboxHook(t)

	schemas, err := h.GetSchemasForSupportedHooks(ctx)
	require.NoError(t, err)
	// We currently return an empty map, but it should be non-nil.
	assert.NotNil(t, schemas)
	assert.Len(t, schemas, 0)
}

func TestUnit_JSSandboxHook_GetToolsForHookByName_OK(t *testing.T) {
	ctx := context.Background()
	h := newTestJSSandboxHook(t)

	tools, err := h.GetToolsForHookByName(ctx, jsSandboxHookName)
	require.NoError(t, err)

	require.Len(t, tools, 1)
	tool := tools[0]

	assert.Equal(t, "function", tool.Type)
	assert.Equal(t, jsSandboxToolName, tool.Function.Name)
	assert.Contains(t, tool.Function.Description, "Executes generated JavaScript")

	// Basic sanity on parameters shape
	params, ok := tool.Function.Parameters.(map[string]any)
	require.True(t, ok, "parameters should be a map[string]any")

	props, ok := params["properties"].(map[string]any)
	require.True(t, ok, "parameters.properties should be a map[string]any")

	codeSchema, ok := props["code"].(map[string]any)
	require.True(t, ok, "parameters.properties.code should be a map[string]any")
	assert.Equal(t, "string", codeSchema["type"])
}

func TestUnit_JSSandboxHook_GetToolsForHookByName_Unknown(t *testing.T) {
	ctx := context.Background()
	h := newTestJSSandboxHook(t)

	tools, err := h.GetToolsForHookByName(ctx, "some_other_hook")
	assert.Error(t, err)
	assert.Nil(t, tools)
}

func TestUnit_JSSandboxHook_Exec_SimpleScript(t *testing.T) {
	ctx := context.Background()
	h := newTestJSSandboxHook(t)

	code := `
		// simple sandbox test
		const result = { value: 42 };
		console.log("hello from js_sandbox", result);
	`

	input := map[string]any{
		"code": code,
	}

	start := time.Now().UTC()
	hookCall := &taskengine.HookCall{
		Name: jsSandboxHookName,
	}

	out, dt, err := h.Exec(ctx, start, input, true, hookCall)
	require.NoError(t, err, "hook Exec should not fail for valid code")
	assert.Equal(t, taskengine.DataTypeJSON, dt, "expected JSON data type from sandbox")

	resp, ok := out.(map[string]any)
	require.True(t, ok, "expected Exec to return map[string]any")

	okFlag, ok := resp["ok"].(bool)
	require.True(t, ok, "ok field should be a bool")
	assert.True(t, okFlag, "ok should be true for successful script")

	codeStr, ok := resp["code"].(string)
	require.True(t, ok, "code field should be a string")
	assert.Contains(t, codeStr, "const result", "code echo should contain original JS snippet")

	resultVal, hasResult := resp["result"]
	require.True(t, hasResult, "result field should be present")

	resultMap, ok := resultVal.(map[string]any)
	require.True(t, ok, "result should be exported as map[string]any")

	// Be tolerant about numeric type; just assert it's 42 as a number.
	val, exists := resultMap["value"]
	require.True(t, exists, "result.value must exist")

	switch v := val.(type) {
	case int:
		assert.Equal(t, 42, v, "result.value should be 42")
	case int64:
		assert.Equal(t, int64(42), v, "result.value should be 42")
	case float64:
		assert.Equal(t, float64(42), v, "result.value should be 42")
	default:
		t.Fatalf("result.value should be numeric 42, got %#v (%T)", v, v)
	}

	logsRaw, hasLogs := resp["logs"]
	require.True(t, hasLogs, "logs field should be present")

	logs, ok := logsRaw.([]jseval.ExecLogEntry)
	require.True(t, ok, "logs should be []jseval.ExecLogEntry")
	require.NotEmpty(t, logs, "logs should not be empty")

	foundConsole := false
	for _, e := range logs {
		if e.Kind == "console" && e.Level == "log" {
			foundConsole = true
			break
		}
	}
	assert.True(t, foundConsole, "expected at least one console log entry in logs")
}

func TestUnit_JSSandboxHook_Exec_RuntimeError_StillOKStruct(t *testing.T) {
	ctx := context.Background()
	h := newTestJSSandboxHook(t)

	// This will throw at runtime (x is not defined).
	code := `
		const result = { value: 1 };
		x.y = 10;
	`

	input := map[string]any{
		"code": code,
	}

	start := time.Now().UTC()
	hookCall := &taskengine.HookCall{
		Name: jsSandboxHookName,
	}

	out, dt, err := h.Exec(ctx, start, input, true, hookCall)
	require.NoError(t, err, "runtime JS error should NOT be a hook-level error")
	assert.Equal(t, taskengine.DataTypeJSON, dt)

	resp, ok := out.(map[string]any)
	require.True(t, ok)

	okFlag, ok := resp["ok"].(bool)
	require.True(t, ok, "ok field should be bool")
	assert.False(t, okFlag, "ok should be false when script has runtime error")

	errStr, _ := resp["error"].(string)
	assert.NotEmpty(t, errStr, "error string should be populated for runtime error")
}

func TestUnit_JSSandboxHook_Exec_EmptyCodeError(t *testing.T) {
	ctx := context.Background()
	h := newTestJSSandboxHook(t)

	start := time.Now().UTC()
	hookCall := &taskengine.HookCall{
		Name: jsSandboxHookName,
	}

	out, dt, err := h.Exec(ctx, start, map[string]any{}, false, hookCall)

	assert.Error(t, err, "expected error for missing code")
	assert.Nil(t, out, "output should be nil on structural error")
	assert.Equal(t, taskengine.DataTypeAny, dt, "should return DataTypeAny on structural error")
}

func TestUnit_JSSandboxHook_Exec_UnknownHookName(t *testing.T) {
	ctx := context.Background()
	h := newTestJSSandboxHook(t)

	start := time.Now().UTC()
	hookCall := &taskengine.HookCall{
		Name: "some_other_hook",
	}

	out, dt, err := h.Exec(ctx, start, map[string]any{"code": "const result = 1;"}, false, hookCall)

	assert.Error(t, err, "expected error for unknown hook name")
	assert.Nil(t, out, "output should be nil on unknown hook")
	assert.Equal(t, taskengine.DataTypeAny, dt, "should return DataTypeAny on unknown hook")
}

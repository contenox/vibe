package localhooks

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/contenox/contenox/runtime/taskengine"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLocalExecHook_Supports(t *testing.T) {
	ctx := context.Background()
	h := NewLocalExecHook().(*LocalExecHook)
	names, err := h.Supports(ctx)
	require.NoError(t, err)
	require.Len(t, names, 1)
	assert.Equal(t, "local_shell", names[0])
}

func TestLocalExecHook_GetSchemasForSupportedHooks(t *testing.T) {
	ctx := context.Background()
	h := NewLocalExecHook().(*LocalExecHook)
	schemas, err := h.GetSchemasForSupportedHooks(ctx)
	require.NoError(t, err)
	require.NotNil(t, schemas)
	require.Contains(t, schemas, "local_shell")
	assert.NotNil(t, schemas["local_shell"])
}

func TestLocalExecHook_GetToolsForHookByName_OK(t *testing.T) {
	ctx := context.Background()
	h := NewLocalExecHook().(*LocalExecHook)
	tools, err := h.GetToolsForHookByName(ctx, "local_shell")
	require.NoError(t, err)
	require.Len(t, tools, 1)
	assert.Equal(t, "function", tools[0].Type)
	assert.Equal(t, "local_shell", tools[0].Function.Name)
	assert.Contains(t, tools[0].Function.Description, "Run a terminal command")
}

func TestLocalExecHook_GetToolsForHookByName_Unknown(t *testing.T) {
	ctx := context.Background()
	h := NewLocalExecHook().(*LocalExecHook)
	tools, err := h.GetToolsForHookByName(ctx, "other")
	assert.Error(t, err)
	assert.Nil(t, tools)
}

func TestLocalExecHook_GetToolsForHookByName_ContextPolicy_Description(t *testing.T) {
	// Hook constructed with NO static policy.
	// Context carries chain-level policy — description must reflect it.
	h := NewLocalExecHook().(*LocalExecHook)
	ctx := taskengine.WithHookArgs(context.Background(), "local_shell", map[string]string{
		"_allowed_commands": "git, ls",
		"_denied_commands":  "rm",
	})
	tools, err := h.GetToolsForHookByName(ctx, "local_shell")
	require.NoError(t, err)
	require.Len(t, tools, 1)
	desc := tools[0].Function.Description
	assert.Contains(t, desc, "git")
	assert.Contains(t, desc, "ls")
	assert.Contains(t, desc, "rm")
}

func TestLocalExecHook_Exec_ContextPolicy_Enforced(t *testing.T) {
	// No static allowlist — context injects one. Command not in list must be rejected.
	h := NewLocalExecHook().(*LocalExecHook)
	ctx := taskengine.WithHookArgs(context.Background(), "local_shell", map[string]string{
		"_allowed_commands": "ls",
	})
	hookCall := &taskengine.HookCall{
		Name: "local_shell",
		Args: map[string]string{"command": "echo", "args": "hello"},
	}
	_, _, err := h.Exec(ctx, time.Now().UTC(), nil, false, hookCall)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not in allowlist")
}

func TestLocalExecHook_Exec_ContextPolicy_Allows(t *testing.T) {
	// No static allowlist — context injects one that includes the command.
	h := NewLocalExecHook().(*LocalExecHook)
	ctx := taskengine.WithHookArgs(context.Background(), "local_shell", map[string]string{
		"_allowed_commands": "echo",
	})
	hookCall := &taskengine.HookCall{
		Name: "local_shell",
		Args: map[string]string{"command": "echo", "args": "ctx policy works"},
	}
	out, dt, err := h.Exec(ctx, time.Now().UTC(), nil, false, hookCall)
	require.NoError(t, err)
	assert.Equal(t, taskengine.DataTypeJSON, dt)
	res, ok := out.(*LocalExecResult)
	require.True(t, ok)
	assert.True(t, res.Success)
	assert.Equal(t, "ctx policy works", res.Stdout)
}

// testAllowedCommands allows the commands used by Exec tests (echo, cat, sleep, shell, exit for shell mode).
var testAllowedCommands = []string{"echo", "cat", "sleep", "/bin/sh", "exit"}

func TestLocalExecHook_Exec_Success(t *testing.T) {
	ctx := context.Background()
	h := NewLocalExecHook(WithLocalExecAllowedCommands(testAllowedCommands)).(*LocalExecHook)
	start := time.Now().UTC()
	hookCall := &taskengine.HookCall{
		Name: "local_shell",
		Args: map[string]string{
			"command": "echo",
			"args":    "hello world",
		},
	}
	out, dt, err := h.Exec(ctx, start, nil, false, hookCall)
	require.NoError(t, err)
	assert.Equal(t, taskengine.DataTypeJSON, dt)
	res, ok := out.(*LocalExecResult)
	require.True(t, ok)
	assert.True(t, res.Success)
	assert.Equal(t, 0, res.ExitCode)
	assert.Equal(t, "hello world", res.Stdout)
	assert.GreaterOrEqual(t, res.DurationSeconds, 0.0)
}

func TestLocalExecHook_Exec_Success_InputAsStdin(t *testing.T) {
	ctx := context.Background()
	h := NewLocalExecHook(WithLocalExecAllowedCommands(testAllowedCommands)).(*LocalExecHook)
	start := time.Now().UTC()
	hookCall := &taskengine.HookCall{
		Name: "local_shell",
		Args: map[string]string{
			"command": "cat",
		},
	}
	out, _, err := h.Exec(ctx, start, "stdin content here", false, hookCall)
	require.NoError(t, err)
	res, ok := out.(*LocalExecResult)
	require.True(t, ok)
	assert.True(t, res.Success)
	assert.Equal(t, "stdin content here", res.Stdout)
}

func TestLocalExecHook_Exec_ShellMode_NoPolicy_Rejected(t *testing.T) {
	// With no policy configured, the default-deny guard fires before shell mode
	// is even attempted. shell:true without an allowlist is never safe.
	ctx := context.Background()
	h := NewLocalExecHook().(*LocalExecHook)
	start := time.Now().UTC()
	hookCall := &taskengine.HookCall{
		Name: "local_shell",
		Args: map[string]string{
			"command": "echo shell test",
			"shell":   "true",
		},
	}
	_, _, err := h.Exec(ctx, start, nil, false, hookCall)
	require.Error(t, err)
	// Default-deny guard fires (no allowlist configured).
	assert.Contains(t, err.Error(), "no allow list configured")
}

func TestLocalExecHook_Exec_ShellMode_WithPolicyRejected(t *testing.T) {
	// shell:true must be REJECTED when an allowlist policy is active to prevent
	// command injection (e.g. "git status; rm -rf /" bypassing allowlist checks).
	ctx := context.Background()
	h := NewLocalExecHook(WithLocalExecAllowedCommands(testAllowedCommands)).(*LocalExecHook)
	start := time.Now().UTC()
	hookCall := &taskengine.HookCall{
		Name: "local_shell",
		Args: map[string]string{
			"command": "echo shell test",
			"shell":   "true",
		},
	}
	_, _, err := h.Exec(ctx, start, nil, false, hookCall)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "strictly forbidden")
}

func TestLocalExecHook_Exec_AllowlistReject(t *testing.T) {
	ctx := context.Background()
	// Only allow /usr/bin/env; echo should be rejected when we use allowedCommands.
	h := NewLocalExecHook(
		WithLocalExecAllowedCommands([]string{"/usr/bin/env"}),
	).(*LocalExecHook)
	start := time.Now().UTC()
	hookCall := &taskengine.HookCall{
		Name: "local_shell",
		Args: map[string]string{
			"command": "echo",
			"args":    "forbidden",
		},
	}
	_, _, err := h.Exec(ctx, start, nil, false, hookCall)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not in allowlist")
}

func TestLocalExecHook_Exec_AllowlistDirReject(t *testing.T) {
	dir := t.TempDir()
	// allowedDir is dir; echo is typically /usr/bin/echo or /bin/echo, not under dir
	h := NewLocalExecHook(
		WithLocalExecAllowedDir(dir),
	).(*LocalExecHook)
	ctx := context.Background()
	start := time.Now().UTC()
	hookCall := &taskengine.HookCall{
		Name: "local_shell",
		Args: map[string]string{"command": "echo", "args": "x"},
	}
	_, _, err := h.Exec(ctx, start, nil, false, hookCall)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not under allowed dir")
}

func TestLocalExecHook_Exec_AllowlistDirAllow(t *testing.T) {
	dir := t.TempDir()
	scriptPath := filepath.Join(dir, "script.sh")
	err := os.WriteFile(scriptPath, []byte("#!/bin/sh\necho ok\n"), 0755)
	require.NoError(t, err)
	h := NewLocalExecHook(
		WithLocalExecAllowedDir(dir),
	).(*LocalExecHook)
	ctx := context.Background()
	start := time.Now().UTC()
	hookCall := &taskengine.HookCall{
		Name: "local_shell",
		Args: map[string]string{"command": scriptPath},
	}
	out, _, err := h.Exec(ctx, start, nil, false, hookCall)
	require.NoError(t, err)
	res, ok := out.(*LocalExecResult)
	require.True(t, ok)
	assert.True(t, res.Success)
	assert.Equal(t, "ok", res.Stdout)
}

func TestLocalExecHook_Exec_Timeout(t *testing.T) {
	ctx := context.Background()
	h := NewLocalExecHook(
		WithLocalExecAllowedCommands(testAllowedCommands),
		WithLocalExecTimeout(50*time.Millisecond),
	).(*LocalExecHook)
	start := time.Now().UTC()
	hookCall := &taskengine.HookCall{
		Name: "local_shell",
		Args: map[string]string{
			"command": "sleep",
			"args":    "2",
			"timeout": "50ms",
		},
	}
	out, _, err := h.Exec(ctx, start, nil, false, hookCall)
	require.NoError(t, err)
	res, ok := out.(*LocalExecResult)
	require.True(t, ok)
	assert.False(t, res.Success)
	// Process is killed on timeout; error may be "context deadline exceeded" or "signal: killed"
	assert.NotEmpty(t, res.Error, "expected some error on timeout")
}

func TestLocalExecHook_Exec_MissingCommand(t *testing.T) {
	ctx := context.Background()
	h := NewLocalExecHook(WithLocalExecAllowedCommands(testAllowedCommands)).(*LocalExecHook)
	hookCall := &taskengine.HookCall{
		Name: "local_shell",
		Args: map[string]string{},
	}
	_, _, err := h.Exec(ctx, time.Now().UTC(), nil, false, hookCall)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "command is required")
}

func TestLocalExecHook_Exec_NilHook(t *testing.T) {
	ctx := context.Background()
	h := NewLocalExecHook().(*LocalExecHook)
	_, _, err := h.Exec(ctx, time.Now().UTC(), nil, false, nil)
	require.Error(t, err)
}

func TestLocalExecHook_Exec_NonZeroExit(t *testing.T) {
	// Run a script under allowedDir WITHOUT shell mode to capture a non-zero exit.
	dir := t.TempDir()
	scriptPath := filepath.Join(dir, "fail.sh")
	err := os.WriteFile(scriptPath, []byte("#!/bin/sh\nexit 3\n"), 0755)
	require.NoError(t, err)
	ctx := context.Background()
	h := NewLocalExecHook(WithLocalExecAllowedDir(dir)).(*LocalExecHook)
	hookCall := &taskengine.HookCall{
		Name: "local_shell",
		Args: map[string]string{"command": scriptPath},
	}
	out, _, err := h.Exec(ctx, time.Now().UTC(), nil, false, hookCall)
	require.NoError(t, err)
	res, ok := out.(*LocalExecResult)
	require.True(t, ok)
	assert.False(t, res.Success)
	assert.Equal(t, 3, res.ExitCode)
}

func TestLocalExecHook_Exec_NonZeroExit_WithPolicy_Rejected(t *testing.T) {
	// shell:true + allowlist must be rejected (security fix).
	ctx := context.Background()
	h := NewLocalExecHook(WithLocalExecAllowedCommands(testAllowedCommands)).(*LocalExecHook)
	hookCall := &taskengine.HookCall{
		Name: "local_shell",
		Args: map[string]string{
			"command": "exit 3",
			"shell":   "true",
		},
	}
	_, _, err := h.Exec(ctx, time.Now().UTC(), nil, false, hookCall)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "strictly forbidden")
}

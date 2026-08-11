package localtools_test

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/contenox/contenox/internal/kernel/taskengine"
	"github.com/contenox/contenox/internal/libsandbox"
	"github.com/contenox/contenox/internal/services/localtools"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type bigOutputRunner struct{ n int }

func (b bigOutputRunner) Run(_ context.Context, _ localtools.CommandSpec, stdout, _ io.Writer) (int, error) {
	_, _ = stdout.Write(make([]byte, b.n))
	return 0, nil
}

func TestUnit_LocalExecTools_CapWriterAppliesRegardlessOfRunner(t *testing.T) {
	h := localtools.NewLocalExecToolsWith(bigOutputRunner{n: 100}).(*localtools.LocalExecTools)
	ctx := context.WithValue(context.Background(), taskengine.ContextKeyOutputByteLimit, int64(10))
	toolsCall := &taskengine.ToolsCall{Name: "local_shell", Args: map[string]string{"command": "anything"}}

	out, dt, err := h.Exec(ctx, time.Now().UTC(), nil, false, toolsCall)
	require.NoError(t, err)
	assert.Equal(t, taskengine.DataTypeJSON, dt)
	res, ok := out.(*localtools.LocalExecResult)
	require.True(t, ok)
	assert.False(t, res.Success, "output over budget must fail regardless of which backend produced it")
	assert.Equal(t, -1, res.ExitCode)
	assert.Contains(t, res.Error, "context budget")
	// capWriter returns the clean head of the stream, not empty.
	assert.NotEmpty(t, res.Stdout)
}

type budgetSignalRunner struct{}

func (budgetSignalRunner) Run(_ context.Context, _ localtools.CommandSpec, stdout, _ io.Writer) (int, error) {
	_, _ = io.WriteString(stdout, "partial output before the backend truncated")
	return -1, localtools.ErrOutputBudgetExceeded
}

func TestUnit_LocalExecTools_BackendSignaledBudgetExceededCircuitBreaks(t *testing.T) {
	h := localtools.NewLocalExecToolsWith(budgetSignalRunner{}).(*localtools.LocalExecTools)
	ctx := context.Background()
	toolsCall := &taskengine.ToolsCall{Name: "local_shell", Args: map[string]string{"command": "anything"}}

	out, dt, err := h.Exec(ctx, time.Now().UTC(), nil, false, toolsCall)
	require.NoError(t, err)
	assert.Equal(t, taskengine.DataTypeJSON, dt)
	res, ok := out.(*localtools.LocalExecResult)
	require.True(t, ok)
	assert.False(t, res.Success, "a backend that truncated server-side must circuit-break exactly like the in-process capWriter")
	assert.Equal(t, -1, res.ExitCode)
	assert.Contains(t, res.Error, "context budget")
	assert.Empty(t, res.Stdout, "partial pre-truncation output must not poison the context window")
}

func TestUnit_LocalExecTools_Supports(t *testing.T) {
	ctx := context.Background()
	h := localtools.NewLocalExecTools().(*localtools.LocalExecTools)
	names, err := h.Supports(ctx)
	require.NoError(t, err)
	require.Len(t, names, 1)
	assert.Equal(t, "local_shell", names[0])
}

func TestUnit_LocalExecTools_GetSchemasForSupportedTools(t *testing.T) {
	ctx := context.Background()
	h := localtools.NewLocalExecTools().(*localtools.LocalExecTools)
	schemas, err := h.GetSchemasForSupportedTools(ctx)
	require.NoError(t, err)
	require.NotNil(t, schemas)
	require.Contains(t, schemas, "local_shell")
	assert.NotNil(t, schemas["local_shell"])
}

func TestUnit_LocalExecTools_GetToolsForToolsByName_OK(t *testing.T) {
	ctx := context.Background()
	h := localtools.NewLocalExecTools().(*localtools.LocalExecTools)
	tools, err := h.GetToolsForToolsByName(ctx, "local_shell")
	require.NoError(t, err)
	require.Len(t, tools, 1)
	assert.Equal(t, "function", tools[0].Type)
	assert.Equal(t, "local_shell", tools[0].Function.Name)
	assert.Contains(t, tools[0].Function.Description, "Run a terminal command")
}

func TestUnit_LocalExecTools_GetToolsForToolsByName_IncludesDetectedShell(t *testing.T) {
	ctx := context.Background()
	h := localtools.NewLocalExecTools(
		localtools.WithLocalExecShell(localtools.NewPowerShellShell("pwsh.exe")),
	).(*localtools.LocalExecTools)

	tools, err := h.GetToolsForToolsByName(ctx, "local_shell")
	require.NoError(t, err)
	require.Len(t, tools, 1)

	desc := tools[0].Function.Description
	assert.Contains(t, desc, "PowerShell")
	assert.Contains(t, desc, "pwsh.exe")
	assert.Contains(t, desc, "$env:NAME")

	params, ok := tools[0].Function.Parameters.(map[string]interface{})
	require.True(t, ok)
	props, ok := params["properties"].(map[string]interface{})
	require.True(t, ok)
	shellProp, ok := props["shell"].(map[string]interface{})
	require.True(t, ok)
	assert.Contains(t, shellProp["description"], "PowerShell")
}

func TestUnit_LocalExecTools_GetSchemasForSupportedTools_IncludesDetectedShell(t *testing.T) {
	ctx := context.Background()
	h := localtools.NewLocalExecTools(
		localtools.WithLocalExecShell(localtools.NewCmdShell("cmd.exe")),
	).(*localtools.LocalExecTools)

	schemas, err := h.GetSchemasForSupportedTools(ctx)
	require.NoError(t, err)

	schema := schemas["local_shell"]
	require.NotNil(t, schema)
	assert.Contains(t, schema.Info.Description, "cmd.exe")
	shellProp := schema.Components.Schemas["LocalExecRequest"].Value.Properties["shell"].Value
	require.NotNil(t, shellProp)
	assert.Contains(t, shellProp.Description, "%NAME%")
}

func TestUnit_LocalExecTools_GetToolsForToolsByName_Unknown(t *testing.T) {
	ctx := context.Background()
	h := localtools.NewLocalExecTools().(*localtools.LocalExecTools)
	tools, err := h.GetToolsForToolsByName(ctx, "other")
	assert.Error(t, err)
	assert.Nil(t, tools)
}

func TestUnit_LocalExecTools_GetToolsForToolsByName_ContextPolicy_Description(t *testing.T) {
	// Tools constructed with NO static policy.
	// Context carries chain-level policy — description must reflect it.
	h := localtools.NewLocalExecTools().(*localtools.LocalExecTools)
	ctx := taskengine.WithToolsArgs(context.Background(), "local_shell", map[string]string{
		"_allowed_commands": "git, ls",
		"_denied_commands":  "rm",
	})
	tools, err := h.GetToolsForToolsByName(ctx, "local_shell")
	require.NoError(t, err)
	require.Len(t, tools, 1)
	desc := tools[0].Function.Description
	assert.Contains(t, desc, "git")
	assert.Contains(t, desc, "ls")
	assert.Contains(t, desc, "rm")

	params, ok := tools[0].Function.Parameters.(map[string]interface{})
	require.True(t, ok)
	props, ok := params["properties"].(map[string]interface{})
	require.True(t, ok)
	shellProp, ok := props["shell"].(map[string]interface{})
	require.True(t, ok)
	assert.Equal(t, "boolean", shellProp["type"])
	assert.NotContains(t, shellProp, "enum", "Gemini rejects boolean enum values in tool declarations")
	assert.Contains(t, shellProp["description"], "Omit or set false")
}

func TestUnit_LocalExecTools_Exec_ContextPolicy_Enforced(t *testing.T) {
	// No static allowlist — context injects one. Command not in list must be rejected.
	h := localtools.NewLocalExecTools().(*localtools.LocalExecTools)
	ctx := taskengine.WithToolsArgs(context.Background(), "local_shell", map[string]string{
		"_allowed_commands": "ls",
	})
	toolsCall := &taskengine.ToolsCall{
		Name: "local_shell",
		Args: map[string]string{"command": "echo", "args": "hello"},
	}
	_, _, err := h.Exec(ctx, time.Now().UTC(), nil, false, toolsCall)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "is not in this chain's allowed commands")
}

func TestUnit_LocalExecTools_Exec_ContextPolicy_Allows(t *testing.T) {
	// No static allowlist — context injects one that includes the command.
	h := localtools.NewLocalExecTools().(*localtools.LocalExecTools)
	ctx := taskengine.WithToolsArgs(context.Background(), "local_shell", map[string]string{
		"_allowed_commands": "echo",
	})
	toolsCall := &taskengine.ToolsCall{
		Name: "local_shell",
		Args: map[string]string{"command": "echo", "args": "ctx policy works"},
	}
	out, dt, err := h.Exec(ctx, time.Now().UTC(), nil, false, toolsCall)
	require.NoError(t, err)
	assert.Equal(t, taskengine.DataTypeJSON, dt)
	res, ok := out.(*localtools.LocalExecResult)
	require.True(t, ok)
	assert.True(t, res.Success)
	assert.Equal(t, "ctx policy works", res.Stdout)
}

// testAllowedCommands allows the commands used by Exec tests (echo, cat, sleep, shell, exit for shell mode).
var testAllowedCommands = []string{"echo", "cat", "sleep", "/bin/sh", "exit"}

func TestUnit_LocalExecTools_Exec_Success(t *testing.T) {
	ctx := context.Background()
	h := localtools.NewLocalExecTools(localtools.WithLocalExecAllowedCommands(testAllowedCommands)).(*localtools.LocalExecTools)
	start := time.Now().UTC()
	toolsCall := &taskengine.ToolsCall{
		Name: "local_shell",
		Args: map[string]string{
			"command": "echo",
			"args":    "hello world",
		},
	}
	out, dt, err := h.Exec(ctx, start, nil, false, toolsCall)
	require.NoError(t, err)
	assert.Equal(t, taskengine.DataTypeJSON, dt)
	res, ok := out.(*localtools.LocalExecResult)
	require.True(t, ok)
	assert.True(t, res.Success)
	assert.Equal(t, 0, res.ExitCode)
	assert.Equal(t, "hello world", res.Stdout)
	assert.GreaterOrEqual(t, res.DurationSeconds, 0.0)
}

func TestUnit_LocalExecTools_Exec_AcceptsJSONDecodedArgsArray(t *testing.T) {
	ctx := context.Background()
	h := localtools.NewLocalExecTools(localtools.WithLocalExecAllowedCommands(testAllowedCommands)).(*localtools.LocalExecTools)

	out, dt, err := h.Exec(ctx, time.Now().UTC(), map[string]any{
		"command": "echo",
		"args":    []any{"json", "array"},
	}, false, &taskengine.ToolsCall{Name: "local_shell"})
	require.NoError(t, err)
	assert.Equal(t, taskengine.DataTypeJSON, dt)
	res, ok := out.(*localtools.LocalExecResult)
	require.True(t, ok)
	assert.True(t, res.Success)
	assert.Equal(t, "json array", res.Stdout)
}

func TestUnit_LocalExecTools_Exec_RejectsUnknownArgs(t *testing.T) {
	h := localtools.NewLocalExecTools(localtools.WithLocalExecAllowedCommands(testAllowedCommands)).(*localtools.LocalExecTools)

	_, _, err := h.Exec(context.Background(), time.Now().UTC(), map[string]any{
		"command": "echo",
		"argv":    "hello",
	}, false, &taskengine.ToolsCall{Name: "local_shell"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown argument")
	assert.Contains(t, err.Error(), "argv")
}

func TestUnit_LocalExecTools_Exec_RejectsNonStringArgsArrayItem(t *testing.T) {
	h := localtools.NewLocalExecTools(localtools.WithLocalExecAllowedCommands(testAllowedCommands)).(*localtools.LocalExecTools)

	_, _, err := h.Exec(context.Background(), time.Now().UTC(), map[string]any{
		"command": "echo",
		"args":    []any{"ok", 123},
	}, false, &taskengine.ToolsCall{Name: "local_shell"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "args[1] must be a string")
}

func TestUnit_LocalExecTools_Exec_Success_InputAsStdin(t *testing.T) {
	ctx := context.Background()
	h := localtools.NewLocalExecTools(localtools.WithLocalExecAllowedCommands(testAllowedCommands)).(*localtools.LocalExecTools)
	start := time.Now().UTC()
	toolsCall := &taskengine.ToolsCall{
		Name: "local_shell",
		Args: map[string]string{
			"command": "cat",
		},
	}
	out, _, err := h.Exec(ctx, start, "stdin content here", false, toolsCall)
	require.NoError(t, err)
	res, ok := out.(*localtools.LocalExecResult)
	require.True(t, ok)
	assert.True(t, res.Success)
	assert.Equal(t, "stdin content here", res.Stdout)
}

func TestUnit_LocalExecTools_Exec_NoPolicy_Allowed(t *testing.T) {
	// Authorization is the responsibility of upstream layers (e.g. HITLWrapper);
	// LocalExecTools without policy must not fail-close.
	ctx := context.Background()
	h := localtools.NewLocalExecTools().(*localtools.LocalExecTools)
	start := time.Now().UTC()
	toolsCall := &taskengine.ToolsCall{
		Name: "local_shell",
		Args: map[string]string{
			"command": "echo",
			"args":    "open posture",
		},
	}
	out, _, err := h.Exec(ctx, start, nil, false, toolsCall)
	require.NoError(t, err)
	res, ok := out.(*localtools.LocalExecResult)
	require.True(t, ok)
	assert.True(t, res.Success)
	assert.Equal(t, "open posture", res.Stdout)
}

func TestUnit_LocalExecTools_Exec_ShellMode_NoPolicy_Allowed(t *testing.T) {
	// shell:true is allowed when no allowlist exists: the injection guard only
	// triggers when there is a policy for shell mode to bypass.
	if runtime.GOOS == "windows" {
		t.Skip("shell:true dispatches to cmd.exe/PowerShell on Windows (see shell.go), not /bin/sh — the exact stdout framing this pins is POSIX-sh-specific")
	}
	ctx := context.Background()
	h := localtools.NewLocalExecTools().(*localtools.LocalExecTools)
	start := time.Now().UTC()
	toolsCall := &taskengine.ToolsCall{
		Name: "local_shell",
		Args: map[string]string{
			"command": "echo shell test",
			"shell":   "true",
		},
	}
	out, _, err := h.Exec(ctx, start, nil, false, toolsCall)
	require.NoError(t, err)
	res, ok := out.(*localtools.LocalExecResult)
	require.True(t, ok)
	assert.True(t, res.Success)
	assert.Equal(t, "shell test", res.Stdout)
}

func TestUnit_LocalExecTools_Exec_ShellMode_WithPolicyRejected(t *testing.T) {
	// shell:true must be REJECTED when an allowlist policy is active to prevent
	// command injection (e.g. "git status; rm -rf /" bypassing allowlist checks).
	ctx := context.Background()
	h := localtools.NewLocalExecTools(localtools.WithLocalExecAllowedCommands(testAllowedCommands)).(*localtools.LocalExecTools)
	start := time.Now().UTC()
	toolsCall := &taskengine.ToolsCall{
		Name: "local_shell",
		Args: map[string]string{
			"command": "echo shell test",
			"shell":   "true",
		},
	}
	_, _, err := h.Exec(ctx, start, nil, false, toolsCall)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "strictly forbidden")
}

func TestUnit_LocalExecTools_Exec_AllowlistReject(t *testing.T) {
	ctx := context.Background()
	// Only allow /usr/bin/env; echo should be rejected when we use allowedCommands.
	h := localtools.NewLocalExecTools(localtools.WithLocalExecAllowedCommands([]string{"/usr/bin/env"})).(*localtools.LocalExecTools)
	start := time.Now().UTC()
	toolsCall := &taskengine.ToolsCall{
		Name: "local_shell",
		Args: map[string]string{
			"command": "echo",
			"args":    "forbidden",
		},
	}
	_, _, err := h.Exec(ctx, start, nil, false, toolsCall)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "is not in this chain's allowed commands")
}

func TestUnit_LocalExecTools_Exec_AllowlistDirReject(t *testing.T) {
	dir := t.TempDir()
	// allowedDir is dir; echo is typically /usr/bin/echo or /bin/echo, not under dir
	h := localtools.NewLocalExecTools(localtools.WithLocalExecAllowedDir(dir)).(*localtools.LocalExecTools)
	ctx := context.Background()
	start := time.Now().UTC()
	toolsCall := &taskengine.ToolsCall{
		Name: "local_shell",
		Args: map[string]string{"command": "echo", "args": "x"},
	}
	_, _, err := h.Exec(ctx, start, nil, false, toolsCall)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not under allowed dir")
}

func TestUnit_LocalExecTools_Exec_AllowlistDirAllow(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("direct-execs a #!/bin/sh script by path; Windows's CreateProcess has no shebang interpretation")
	}
	dir := t.TempDir()
	scriptPath := filepath.Join(dir, "script.sh")
	err := os.WriteFile(scriptPath, []byte("#!/bin/sh\necho ok\n"), 0755)
	require.NoError(t, err)
	h := localtools.NewLocalExecTools(localtools.WithLocalExecAllowedDir(dir)).(*localtools.LocalExecTools)
	ctx := context.Background()
	start := time.Now().UTC()
	toolsCall := &taskengine.ToolsCall{
		Name: "local_shell",
		Args: map[string]string{"command": scriptPath},
	}
	out, _, err := h.Exec(ctx, start, nil, false, toolsCall)
	require.NoError(t, err)
	res, ok := out.(*localtools.LocalExecResult)
	require.True(t, ok)
	assert.True(t, res.Success)
	assert.Equal(t, "ok", res.Stdout)
}

func TestUnit_LocalExecTools_Exec_Timeout(t *testing.T) {
	ctx := context.Background()
	h := localtools.NewLocalExecTools(localtools.WithLocalExecAllowedCommands(testAllowedCommands), localtools.WithLocalExecTimeout(50*time.Millisecond)).(*localtools.LocalExecTools)
	start := time.Now().UTC()
	toolsCall := &taskengine.ToolsCall{
		Name: "local_shell",
		Args: map[string]string{
			"command": "sleep",
			"args":    "2",
			"timeout": "50ms",
		},
	}
	out, _, err := h.Exec(ctx, start, nil, false, toolsCall)
	require.NoError(t, err)
	res, ok := out.(*localtools.LocalExecResult)
	require.True(t, ok)
	assert.False(t, res.Success)
	// Process is killed on timeout; error may be "context deadline exceeded" or "signal: killed"
	assert.NotEmpty(t, res.Error, "expected some error on timeout")
}

func TestUnit_LocalExecTools_Exec_NoTrimWhitespace(t *testing.T) {
	ctx := context.Background()
	h := localtools.NewLocalExecTools().(*localtools.LocalExecTools)
	start := time.Now().UTC()
	out, _, err := h.Exec(ctx, start, map[string]any{
		"command": "echo",
		"args":    []any{"  spaced  "},
	}, false, &taskengine.ToolsCall{Name: "local_shell"})
	require.NoError(t, err)
	res, ok := out.(*localtools.LocalExecResult)
	require.True(t, ok)
	assert.True(t, res.Success)
	// A trailing \n is removed; leading/trailing spaces are preserved.
	assert.Equal(t, "  spaced  ", res.Stdout)
}

func TestUnit_LocalExecTools_Exec_MissingCommand(t *testing.T) {
	ctx := context.Background()
	h := localtools.NewLocalExecTools(localtools.WithLocalExecAllowedCommands(testAllowedCommands)).(*localtools.LocalExecTools)
	toolsCall := &taskengine.ToolsCall{
		Name: "local_shell",
		Args: map[string]string{},
	}
	_, _, err := h.Exec(ctx, time.Now().UTC(), nil, false, toolsCall)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "command is required")
}

func TestUnit_LocalExecTools_Exec_NilTools(t *testing.T) {
	ctx := context.Background()
	h := localtools.NewLocalExecTools().(*localtools.LocalExecTools)
	_, _, err := h.Exec(ctx, time.Now().UTC(), nil, false, nil)
	require.Error(t, err)
}

func TestUnit_LocalExecTools_Exec_NonZeroExit(t *testing.T) {
	// Run a script under allowedDir WITHOUT shell mode to capture a non-zero exit.
	if runtime.GOOS == "windows" {
		t.Skip("direct-execs a #!/bin/sh script by path; Windows's CreateProcess has no shebang interpretation")
	}
	dir := t.TempDir()
	scriptPath := filepath.Join(dir, "fail.sh")
	err := os.WriteFile(scriptPath, []byte("#!/bin/sh\nexit 3\n"), 0755)
	require.NoError(t, err)
	ctx := context.Background()
	h := localtools.NewLocalExecTools(localtools.WithLocalExecAllowedDir(dir)).(*localtools.LocalExecTools)
	toolsCall := &taskengine.ToolsCall{
		Name: "local_shell",
		Args: map[string]string{"command": scriptPath},
	}
	out, _, err := h.Exec(ctx, time.Now().UTC(), nil, false, toolsCall)
	require.NoError(t, err)
	res, ok := out.(*localtools.LocalExecResult)
	require.True(t, ok)
	assert.False(t, res.Success)
	assert.Equal(t, 3, res.ExitCode)
}

// TestUnit_LocalExecTools_ScrubEnv_StripsSecretKeepsToolchain pins the
// credential-leak fix: with the default agent-shell posture (deny-secrets)
// wired via WithLocalExecScrubEnv, a spawned command must not see a
// credential-shaped variable from the process environment, while PATH/HOME
// (needed by any real toolchain command) survive.
func TestUnit_LocalExecTools_ScrubEnv_StripsSecretKeepsToolchain(t *testing.T) {
	t.Setenv("TESTSECRET_API_KEY", "leaked-value")
	t.Setenv("HOME", "/home/scrub-test")

	scrub := libsandbox.EnvScrub(libsandbox.ScrubDenySecrets, nil, nil)
	h := localtools.NewLocalExecTools(localtools.WithLocalExecScrubEnv(scrub)).(*localtools.LocalExecTools)

	ctx := context.Background()
	toolsCall := &taskengine.ToolsCall{
		Name: "local_shell",
		Args: map[string]string{"command": "env"},
	}
	out, _, err := h.Exec(ctx, time.Now().UTC(), nil, false, toolsCall)
	require.NoError(t, err)
	res, ok := out.(*localtools.LocalExecResult)
	require.True(t, ok)
	require.True(t, res.Success, "env: %s", res.Error)
	assert.NotContains(t, res.Stdout, "TESTSECRET_API_KEY", "the default scrub must strip credential-shaped names")
	assert.True(t, strings.Contains(res.Stdout, "PATH="), "PATH must survive the default scrub or every spawned shell breaks")
	assert.True(t, strings.Contains(res.Stdout, "HOME=/home/scrub-test"), "HOME must survive the default scrub (Allow=\"*\" under deny-secrets)")
}

func TestUnit_LocalExecTools_Exec_NonZeroExit_WithPolicy_Rejected(t *testing.T) {
	// shell:true + allowlist must be rejected (security fix).
	ctx := context.Background()
	h := localtools.NewLocalExecTools(localtools.WithLocalExecAllowedCommands(testAllowedCommands)).(*localtools.LocalExecTools)
	toolsCall := &taskengine.ToolsCall{
		Name: "local_shell",
		Args: map[string]string{
			"command": "exit 3",
			"shell":   "true",
		},
	}
	_, _, err := h.Exec(ctx, time.Now().UTC(), nil, false, toolsCall)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "strictly forbidden")
}

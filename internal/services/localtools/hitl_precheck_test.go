package localtools_test

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/contenox/contenox/internal/kernel/taskengine"
	"github.com/contenox/contenox/internal/services/hitlservice"
	"github.com/contenox/contenox/internal/services/localtools"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// shellCall builds the local_shell call shape the engine emits.
func shellCall(command string) *taskengine.ToolsCall {
	return &taskengine.ToolsCall{
		Name:     "local_shell",
		ToolName: "local_shell",
		Args:     map[string]string{"command": command},
	}
}

// withAllowedCommands injects a chain's _allowed_commands the way ExecEnv does.
func withAllowedCommands(ctx context.Context, list string) context.Context {
	return taskengine.WithToolsArgs(ctx, "local_shell", map[string]string{
		"_allowed_commands": list,
	})
}

// TestUnit_HITLWrapper_UnrunnableCommandIsNeverAsked is the invariant this seam
// exists for: a command the tool will refuse must be refused BEFORE a human is
// asked. The failure it guards is a gate that reverses itself — an operator
// approves a card and the call is refused anyway — which spends human attention
// on a decision that changes nothing.
func TestUnit_HITLWrapper_UnrunnableCommandIsNeverAsked(t *testing.T) {
	inner := localtools.NewLocalExecToolsWith(refusingRunner{t: t})

	asked := 0
	ask := func(context.Context, hitlservice.ApprovalRequest) (bool, error) {
		asked++
		return true, nil
	}
	w := localtools.NewHITLWrapper(inner, ask, approvePolicy(), nil)

	ctx := withAllowedCommands(context.Background(), "git,go")
	_, _, err := w.Exec(ctx, time.Now(), nil, false, shellCall("kubectl"))

	require.Error(t, err, "a command outside the allowlist must be refused")
	assert.Zero(t, asked, "the human was asked to approve a command that cannot run")
	assert.Contains(t, err.Error(), "kubectl")
	assert.Contains(t, err.Error(), "_allowed_commands",
		"the refusal must name where the list is configured")
	assert.Contains(t, err.Error(), "no approval can grant it",
		"the refusal must say that approving again cannot help")
}

// TestUnit_HITLWrapper_RunnableCommandIsStillAsked is the other half: the
// precheck must not swallow the gate. A command that passes the allowlist still
// reaches the human exactly as before.
func TestUnit_HITLWrapper_RunnableCommandIsStillAsked(t *testing.T) {
	inner := localtools.NewLocalExecToolsWith(okRunner{})

	asked := 0
	ask := func(context.Context, hitlservice.ApprovalRequest) (bool, error) {
		asked++
		return true, nil
	}
	w := localtools.NewHITLWrapper(inner, ask, approvePolicy(), nil)

	ctx := withAllowedCommands(context.Background(), "git,go")
	_, _, err := w.Exec(ctx, time.Now(), nil, false, shellCall("git"))

	require.NoError(t, err)
	assert.Equal(t, 1, asked, "an allowed command must still be gated by the human")
}

// TestUnit_HITLWrapper_PrecheckIsOptional pins that an inner repo without the
// seam is gated exactly as it was. Absent means absent.
func TestUnit_HITLWrapper_PrecheckIsOptional(t *testing.T) {
	inner := &mockInnerTools{}
	asked := 0
	ask := func(context.Context, hitlservice.ApprovalRequest) (bool, error) {
		asked++
		return true, nil
	}
	w := localtools.NewHITLWrapper(inner, ask, approvePolicy(), nil)

	_, _, err := w.Exec(context.Background(), time.Now(), map[string]any{"path": "a.txt"}, false,
		&taskengine.ToolsCall{Name: "local_fs", ToolName: "read_file"})

	require.NoError(t, err)
	assert.Equal(t, 1, asked)
	assert.Len(t, inner.calls, 1)
}

// TestUnit_LocalExecTools_ExecStillEnforcesTheAllowlist pins the check as
// defense in depth. Precheck is an early copy; the tool must hold its own
// boundary when nothing wraps it.
func TestUnit_LocalExecTools_ExecStillEnforcesTheAllowlist(t *testing.T) {
	h := localtools.NewLocalExecToolsWith(refusingRunner{t: t})

	ctx := withAllowedCommands(context.Background(), "git,go")
	_, _, err := h.Exec(ctx, time.Now(), nil, false, shellCall("kubectl"))

	require.Error(t, err)
	assert.True(t, strings.Contains(err.Error(), "kubectl"))
}

// refusingRunner fails the test if anything actually executes.
type refusingRunner struct{ t *testing.T }

func (r refusingRunner) Run(_ context.Context, spec localtools.CommandSpec, _, _ io.Writer) (int, error) {
	r.t.Fatalf("a refused command reached the runner: %v", spec)
	return 0, nil
}

// okRunner succeeds without doing anything.
type okRunner struct{}

func (okRunner) Run(_ context.Context, _ localtools.CommandSpec, _, _ io.Writer) (int, error) {
	return 0, nil
}

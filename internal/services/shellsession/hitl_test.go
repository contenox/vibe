//go:build !windows

package shellsession

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/contenox/beam/internal/kernel/taskengine"
	"github.com/contenox/beam/internal/services/hitlservice"
	"github.com/contenox/beam/internal/services/localtools"
)

// fakePolicy mirrors the intended default policy shape: shell_session_read is
// always allowed (reference-only reads are never gated), while shell_session_run
// takes the configured action so a test can flip between "strict" (approve) and
// "permissive" (allow).
type fakePolicy struct{ runAction hitlservice.Action }

func (f fakePolicy) Evaluate(_ context.Context, _, toolName string, _ map[string]any) (hitlservice.EvaluationResult, error) {
	if toolName == ToolRead {
		return hitlservice.EvaluationResult{Action: hitlservice.ActionAllow}, nil
	}
	return hitlservice.EvaluationResult{Action: f.runAction}, nil
}

func runCall(name string) *taskengine.ToolsCall {
	return &taskengine.ToolsCall{Name: ToolsProviderName, ToolName: name}
}

func TestShellTools_RunIsHITLGated(t *testing.T) {
	m := newTestManager(t, time.Minute)
	ctx := ctxWithSession("hitl-sess")

	// Strict policy: run requires approval. A denied approval must NOT execute.
	var asked int
	deny := func(context.Context, hitlservice.ApprovalRequest) (bool, error) { asked++; return false, nil }
	strict := localtools.NewHITLWrapper(NewTools(m), deny, fakePolicy{runAction: hitlservice.ActionApprove}, nil)

	out, _, err := strict.Exec(ctx, time.Now(), map[string]any{"command": "echo denied-line"}, false, runCall(ToolRun))
	if err != nil {
		t.Fatalf("strict run exec: %v", err)
	}
	if asked != 1 {
		t.Fatalf("strict policy must ask for approval exactly once, asked=%d", asked)
	}
	if s, _ := out.(string); s != localtools.DenyMessage {
		t.Fatalf("denied run must return the deny message, got %T %v", out, out)
	}
	// Nothing should have been typed into a shell.
	if r := m.Read("hitl-sess", 0, 0); r.Exists && strings.Contains(r.Content, "denied-line") {
		t.Fatalf("denied command must not have executed; scrollback=%q", r.Content)
	}
}

func TestShellTools_PermissiveRunsAndReadReturnsOutput(t *testing.T) {
	m := newTestManager(t, time.Minute)
	ctx := ctxWithSession("perm-sess")

	var asked int
	ask := func(context.Context, hitlservice.ApprovalRequest) (bool, error) { asked++; return true, nil }
	permissive := localtools.NewHITLWrapper(NewTools(m), ask, fakePolicy{runAction: hitlservice.ActionAllow}, nil)

	if _, _, err := permissive.Exec(ctx, time.Now(), map[string]any{"command": "echo permit-line"}, false, runCall(ToolRun)); err != nil {
		t.Fatalf("permissive run exec: %v", err)
	}
	if asked != 0 {
		t.Fatalf("permissive policy must not ask for approval, asked=%d", asked)
	}

	// The read tool is ungated and returns what the run wrote.
	var content string
	ok := waitFor(t, 3*time.Second, func() bool {
		out, _, err := permissive.Exec(ctx, time.Now(), map[string]any{}, false, runCall(ToolRead))
		if err != nil {
			t.Fatalf("read exec: %v", err)
		}
		r, _ := out.(ReadResultJSON)
		content = r.Content
		return strings.Contains(content, "permit-line")
	})
	if asked != 0 {
		t.Fatalf("read must be ungated (no approval), asked=%d", asked)
	}
	if !ok {
		t.Fatalf("read did not return the run's output; scrollback=%q", content)
	}
}

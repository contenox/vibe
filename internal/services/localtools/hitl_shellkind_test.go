package localtools_test

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/contenox/contenox/internal/kernel/taskengine"
	"github.com/contenox/contenox/internal/services/hitlservice"
	"github.com/contenox/contenox/internal/services/localtools"
)

// H1: the structural shell reader only runs on a grammar someone positively
// established. hitlservice cannot establish it — it would have to infer the
// shell from GOOS and from a model-supplied "shell_kind" argument — so this
// package, which spawns the shell, declares it. These tests exercise the
// declaration through the real wrapper and a real policy, not a ctx shortcut.

// shellKindKV is the process-global active-policy reader hitlservice.New
// needs; the tests pin one policy file.
type shellKindKV struct{ name string }

func (k shellKindKV) GetKV(_ context.Context, _ string, out interface{}) error {
	if p, ok := out.(*string); ok {
		*p = k.name
	}
	return nil
}

const shellKindSafeVerbs = "git status,git log,go build,go test,ls,cat,echo"

// compoundLinePolicy is the shipped tier shape reduced to what this case
// needs: an allow tier over safe verbs, an approve floor.
func compoundLinePolicy(t *testing.T) hitlservice.PolicyEvaluator {
	t.Helper()
	dir := t.TempDir()
	body := []byte(`{"default_action":"approve","rules":[
		{"tools":"local_shell","tool":"local_shell","action":"allow","when":[{"key":"command","op":"command_prefix_allowlist","value":"` + shellKindSafeVerbs + `"}]}
	]}`)
	if err := os.WriteFile(filepath.Join(dir, "hitl-policy.json"), body, 0o644); err != nil {
		t.Fatalf("write policy: %v", err)
	}
	return hitlservice.New(hitlservice.NewFSPolicySource(dir), "00000000-0000-0000-0000-000000000001",
		shellKindKV{"hitl-policy.json"}, nil)
}

// runShellCall drives one local_shell call through the wrapper and reports
// whether the human was asked.
func runShellCall(t *testing.T, w *localtools.HITLWrapper, inner *mockInnerTools, args map[string]any) (asked bool) {
	t.Helper()
	before := len(inner.calls)
	_, _, err := w.Exec(context.Background(), time.Now(), args, false,
		&taskengine.ToolsCall{Name: "local_shell", ToolName: "local_shell"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// alwaysDeny is the ask callback, so an ask never reaches inner.
	return len(inner.calls) == before
}

// TestUnit_HITLWrapper_DeclaresPOSIXShell_CompoundLineStopsAsking is the H1
// win as an operator experiences it: two allowlisted verbs joined by && no
// longer raise an approval card. Platform-independent because the shell is
// declared explicitly rather than inferred.
func TestUnit_HITLWrapper_DeclaresPOSIXShell_CompoundLineStopsAsking(t *testing.T) {
	inner := &mockInnerTools{}
	w := localtools.NewHITLWrapper(inner, alwaysDeny, compoundLinePolicy(t), nil)
	w.SetShell(localtools.NewShShell(""))

	for _, cmd := range []string{`git status && go build`, `git log --oneline | cat`} {
		if runShellCall(t, w, inner, map[string]any{"command": cmd}) {
			t.Errorf("%q is entirely allowlisted verbs and must not interrupt the operator", cmd)
		}
	}
}

// TestUnit_HITLWrapper_DeclaredShellBeatsAModelSuppliedHint pins why the
// declaration has to come from here: without it, a "shell_kind" argument in
// the call decides whether the security analyzer runs at all.
func TestUnit_HITLWrapper_DeclaredShellBeatsAModelSuppliedHint(t *testing.T) {
	inner := &mockInnerTools{}
	w := localtools.NewHITLWrapper(inner, alwaysDeny, compoundLinePolicy(t), nil)
	w.SetShell(localtools.NewShShell(""))

	if runShellCall(t, w, inner, map[string]any{"command": "git status && go build", "shell_kind": "powershell"}) {
		t.Error("a model-supplied shell_kind must not decide whether the structural reader runs")
	}
}

// TestUnit_HITLWrapper_NonPOSIXShellNeverUpgrades is A1 through the real
// wiring: mvdan parses POSIX, not PowerShell or cmd, so a host that spawns
// either keeps the tokenizer's verdict and the compound-line win does not
// reach it.
func TestUnit_HITLWrapper_NonPOSIXShellNeverUpgrades(t *testing.T) {
	for name, shell := range map[string]localtools.PlatformShell{
		"powershell": localtools.NewPowerShellShell(""),
		"cmd":        localtools.NewCmdShell(""),
	} {
		inner := &mockInnerTools{}
		w := localtools.NewHITLWrapper(inner, alwaysDeny, compoundLinePolicy(t), nil)
		w.SetShell(shell)
		if !runShellCall(t, w, inner, map[string]any{"command": "git status && go build"}) {
			t.Errorf("%s: a non-POSIX line must never be read as POSIX shell", name)
		}
		// The single-command tokenizer verdict is unchanged there.
		if runShellCall(t, w, inner, map[string]any{"command": "git", "args": []any{"status"}}) {
			t.Errorf("%s: a plain argv call keeps today's allow", name)
		}
	}
}

// TestUnit_HITLWrapper_DetectedShellMatchesTheHost pins the default the
// constructor picks: POSIX everywhere local_shell can only spawn sh, and
// fail-closed on Windows, where the analyzer is not cleared.
func TestUnit_HITLWrapper_DetectedShellMatchesTheHost(t *testing.T) {
	inner := &mockInnerTools{}
	w := localtools.NewHITLWrapper(inner, alwaysDeny, compoundLinePolicy(t), nil)

	asked := runShellCall(t, w, inner, map[string]any{"command": "git status && go build"})
	if runtime.GOOS == "windows" {
		if !asked {
			t.Error("windows: local_shell spawns PowerShell or cmd, so a compound line must keep asking")
		}
		return
	}
	if asked {
		t.Error("non-windows: local_shell can only spawn sh, so a compound allowlisted line must not ask")
	}
}

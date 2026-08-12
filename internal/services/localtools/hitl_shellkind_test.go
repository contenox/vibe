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

type shellKindKV struct{ name string }

func (k shellKindKV) GetKV(_ context.Context, _ string, out interface{}) error {
	if p, ok := out.(*string); ok {
		*p = k.name
	}
	return nil
}

const shellKindSafeVerbs = "git status,git log,go build,go test,ls,cat,echo"

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

// TestUnit_HITLWrapper_DeclaresPOSIXShell_CompoundLineStopsAsking pins that two allowlisted verbs joined by && or | do not raise an approval card, since the shell is declared explicitly rather than inferred.
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

// TestUnit_HITLWrapper_DeclaredShellBeatsAModelSuppliedHint pins that a model-supplied "shell_kind" argument never decides whether the structural security analyzer runs.
func TestUnit_HITLWrapper_DeclaredShellBeatsAModelSuppliedHint(t *testing.T) {
	inner := &mockInnerTools{}
	w := localtools.NewHITLWrapper(inner, alwaysDeny, compoundLinePolicy(t), nil)
	w.SetShell(localtools.NewShShell(""))

	if runShellCall(t, w, inner, map[string]any{"command": "git status && go build", "shell_kind": "powershell"}) {
		t.Error("a model-supplied shell_kind must not decide whether the structural reader runs")
	}
}

// TestUnit_HITLWrapper_NonPOSIXShellNeverUpgrades pins that mvdan parses POSIX only, so a host spawning PowerShell or cmd keeps the single-command tokenizer's verdict and never gets the compound-line allowlist win.
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

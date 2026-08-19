package gointel

import (
	"context"
	"testing"
	"time"

	"github.com/contenox/contenox/internal/kernel/taskengine"
)

// withPolicy is the chain-level tools policy as the engine delivers it: keyed on
// the toolset's registration name, never merged into the model's arguments.
func withPolicy(args map[string]string) context.Context {
	return taskengine.WithToolsArgs(context.Background(), ToolsProviderName, args)
}

func execPolicyTool(t *testing.T, ctx context.Context, repo taskengine.ToolsRepo, tool string, args map[string]any) any {
	t.Helper()
	out, _, err := repo.Exec(ctx, time.Now(), args, false,
		&taskengine.ToolsCall{Name: ToolsProviderName, ToolName: tool})
	if err != nil {
		t.Fatalf("%s(%v): %v", tool, args, err)
	}
	return out
}

func TestUnit_Policy_NoArgsLeavesThePackageCeilings(t *testing.T) {
	got := limitsFrom(context.Background(), ToolsProviderName)
	want := limits{references: maxRefCap, symbols: maxSymbolCap, diagnostics: maxDiagCap}
	if got != want {
		t.Fatalf("limitsFrom(no args) = %+v, want %+v", got, want)
	}
}

func TestUnit_Policy_TightensButNeverWidens(t *testing.T) {
	for _, tc := range []struct {
		name string
		args map[string]string
		want limits
	}{
		{
			"below the ceiling is honoured",
			map[string]string{policyMaxReferences: "5", policyMaxSymbols: "7", policyMaxDiagnostics: "9"},
			limits{references: 5, symbols: 7, diagnostics: 9},
		},
		{
			"above the ceiling clamps down, so policy cannot widen the toolset",
			map[string]string{policyMaxReferences: "99999", policyMaxSymbols: "99999", policyMaxDiagnostics: "99999"},
			limits{references: maxRefCap, symbols: maxSymbolCap, diagnostics: maxDiagCap},
		},
		{
			"zero and negative floor at one rather than denying every result",
			map[string]string{policyMaxReferences: "0", policyMaxSymbols: "-1", policyMaxDiagnostics: "-100"},
			limits{references: 1, symbols: 1, diagnostics: 1},
		},
		{
			"unparseable values fall back to the ceiling instead of failing the call",
			map[string]string{policyMaxReferences: "many", policyMaxSymbols: "", policyMaxDiagnostics: "1.5"},
			limits{references: maxRefCap, symbols: maxSymbolCap, diagnostics: maxDiagCap},
		},
		{
			"surrounding whitespace is tolerated",
			map[string]string{policyMaxReferences: "  4\n"},
			limits{references: 4, symbols: maxSymbolCap, diagnostics: maxDiagCap},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := limitsFrom(withPolicy(tc.args), ToolsProviderName); got != tc.want {
				t.Fatalf("limitsFrom(%v) = %+v, want %+v", tc.args, got, tc.want)
			}
		})
	}
}

// The policy block is addressed by the registration key, so args parked under
// any other toolset name are another toolset's policy and must not bind here.
func TestUnit_Policy_ArgsAreKeyedOnTheRegisteredToolsetName(t *testing.T) {
	for _, name := range []string{"gointel", "native-goja", "local_fs", ""} {
		ctx := taskengine.WithToolsArgs(context.Background(), name, map[string]string{policyMaxReferences: "1"})
		if got := limitsFrom(ctx, ToolsProviderName); got.references != maxRefCap {
			t.Errorf("policy written under %q bound to %q: references = %d", name, ToolsProviderName, got.references)
		}
	}
}

func TestUnit_Policy_ClampMaxLeavesTheIndexDefaultAloneUntilPolicyCutsBelowIt(t *testing.T) {
	for _, tc := range []struct {
		name                    string
		requested, ceiling, def int
		want                    int
	}{
		{"unset max stays unset while the default fits", 0, 200, 50, 0},
		{"unset max becomes the ceiling once policy cuts under the default", 0, 10, 50, 10},
		{"a request under the ceiling passes through", 30, 200, 50, 30},
		{"a request over the ceiling is cut to it", 500, 200, 50, 200},
		{"the model cannot outbid a tightened ceiling", 200, 3, 50, 3},
		{"a negative request is treated as unset", -7, 200, 50, -7},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := clampMax(tc.requested, tc.ceiling, tc.def); got != tc.want {
				t.Fatalf("clampMax(%d, %d, %d) = %d, want %d", tc.requested, tc.ceiling, tc.def, got, tc.want)
			}
		})
	}
}

// End to end through Exec: the ceiling reaches the query only via the context,
// and the model's own max cannot climb back over it.
func TestUnit_Policy_ReferenceCeilingReachesTheQuery(t *testing.T) {
	repo, _ := newTestTools(t)

	// shapes.Unit is the fixture's reference magnet, used in both packages.
	unpoliced := execPolicyTool(t, context.Background(), repo, ToolReferences,
		map[string]any{"symbol": "shapes.Unit"}).(*ReferencesResult)
	if unpoliced.Total < 3 {
		t.Fatalf("fixture symbol has only %d references; the ceiling test needs several", unpoliced.Total)
	}
	if unpoliced.Shown != unpoliced.Total {
		t.Fatalf("without a policy the default cap already truncated: shown %d of %d", unpoliced.Shown, unpoliced.Total)
	}

	policed := execPolicyTool(t, withPolicy(map[string]string{policyMaxReferences: "1"}), repo, ToolReferences,
		map[string]any{"symbol": "shapes.Unit"}).(*ReferencesResult)
	if policed.Shown != 1 {
		t.Fatalf("policy ceiling 1: shown = %d, want 1", policed.Shown)
	}
	if policed.Total != unpoliced.Total {
		t.Errorf("the ceiling changed the reported total: %d, want %d", policed.Total, unpoliced.Total)
	}
	if policed.Note == "" {
		t.Error("a truncated result carries no note saying so")
	}

	// The model asking for the package ceiling does not lift the chain's.
	overbid := execPolicyTool(t, withPolicy(map[string]string{policyMaxReferences: "1"}), repo, ToolReferences,
		map[string]any{"symbol": "shapes.Unit", "max": maxRefCap}).(*ReferencesResult)
	if overbid.Shown != 1 {
		t.Fatalf("model max=%d beat the policy ceiling: shown = %d, want 1", maxRefCap, overbid.Shown)
	}
}

func TestUnit_Policy_SymbolCeilingReachesTheQuery(t *testing.T) {
	repo, _ := newTestTools(t)

	unpoliced := execPolicyTool(t, context.Background(), repo, ToolSymbols,
		map[string]any{"target": "shapes"}).(*SymbolsResult)
	if unpoliced.Total < 3 {
		t.Fatalf("fixture package outlines only %d symbols; the ceiling test needs several", unpoliced.Total)
	}

	policed := execPolicyTool(t, withPolicy(map[string]string{policyMaxSymbols: "2"}), repo, ToolSymbols,
		map[string]any{"target": "shapes", "max": 500}).(*SymbolsResult)
	if policed.Shown != 2 {
		t.Fatalf("policy ceiling 2: shown = %d, want 2", policed.Shown)
	}
	if policed.Total != unpoliced.Total {
		t.Errorf("the ceiling changed the reported total: %d, want %d", policed.Total, unpoliced.Total)
	}
}

func TestUnit_Policy_DiagnosticCeilingReachesTheQuery(t *testing.T) {
	repo := NewTools(newTestIndex(t, newFixture(t, "badfixture")))

	policed := execPolicyTool(t, withPolicy(map[string]string{policyMaxDiagnostics: "1"}), repo, ToolDiagnostics,
		map[string]any{"scope": "all", "max": maxDiagCap}).(*DiagnosticsResult)
	if policed.Total < 2 {
		t.Fatalf("badfixture produced %d diagnostics; the ceiling test needs several", policed.Total)
	}
	if policed.Shown != 1 {
		t.Fatalf("policy ceiling 1: shown = %d, want 1", policed.Shown)
	}
}

package searchtool

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/contenox/contenox/internal/kernel/taskengine"
)

// universe is what PersistentRepo.Supports hands the allowlist: this toolset's
// registered key alongside an unscoped operator registration and a declared MCP
// row.
func universe() []string {
	return []string{ToolsProviderName, "local_fs", "decl-reviewer-github"}
}

func admits(allowlist []string, name string) bool {
	for _, got := range taskengine.ExportedApplyAllowlist(allowlist, universe()) {
		if got == name {
			return true
		}
	}
	return false
}

// TestUnit_Gate_StarAdmitsScopedNameBangRemovesIt pins the allowlist vocabulary: "*" admits every connected toolset with no exceptions, "!name" removes one, a bare name grants exactly it, an empty allowlist grants nothing.
func TestUnit_Gate_StarAdmitsScopedNameBangRemovesIt(t *testing.T) {
	// native- is a namespace, so a declared MCP source cannot mint this key.
	if !strings.HasPrefix(ToolsProviderName, "native-") {
		t.Fatalf("provider name %q dropped the native- namespace; a declared source could collide with it", ToolsProviderName)
	}

	if !admits([]string{"*"}, ToolsProviderName) {
		t.Errorf("%q must be admitted by \"*\": the scope is a namespace, not a hidden exclusion", ToolsProviderName)
	}
	if !admits([]string{"*"}, "local_fs") {
		t.Error("the wildcard no longer admits unscoped toolsets")
	}
	if !admits([]string{"*"}, "decl-reviewer-github") {
		t.Error("the wildcard must admit a declared MCP row too; \"*\" means everything")
	}

	if !admits([]string{ToolsProviderName}, ToolsProviderName) {
		t.Errorf("%q is not admitted when a declaration names it exactly", ToolsProviderName)
	}
	if admits([]string{ToolsProviderName}, "local_fs") {
		t.Error("a bare name granted more than exactly itself")
	}
	if admits([]string{"*", "!" + ToolsProviderName}, ToolsProviderName) {
		t.Errorf("%q survives \"!\" under the wildcard; an operator cannot remove one set", ToolsProviderName)
	}
	if !admits([]string{"*", "!" + ToolsProviderName}, "local_fs") {
		t.Error("removing one set removed the others with it")
	}
	if admits([]string{ToolsProviderName, "!" + ToolsProviderName}, ToolsProviderName) {
		t.Errorf("%q survives its own denial entry", ToolsProviderName)
	}
	if admits(nil, ToolsProviderName) {
		t.Errorf("%q is admitted with no allowlist at all", ToolsProviderName)
	}
}

// The gate keys on the toolset name, so the name the policy block, the HITL
// rules and the registration all use must be the one Supports reports.
func TestUnit_Gate_RegisteredNameIsTheNameSupportsReports(t *testing.T) {
	got, err := newTools(&fakeQuerier{}).Supports(context.Background())
	if err != nil {
		t.Fatalf("Supports: %v", err)
	}
	if len(got) == 0 || got[0] != ToolsProviderName {
		t.Fatalf("Supports() = %v, want it to lead with %q", got, ToolsProviderName)
	}
	// The tool itself is NOT prefixed: only toolset names reach the allowlist,
	// and workspace_search is the seeded HITL policy key.
	if strings.HasPrefix(ToolSearch, "native-") {
		t.Errorf("tool name %q is prefixed; the namespace scopes toolsets, not tools", ToolSearch)
	}
}

// The descriptor is what an admitted toolset actually costs, so it is reachable
// under the registered key — the same key PersistentRepo dispatches on.
func TestUnit_Gate_DescriptorIsReachableUnderTheRegisteredKey(t *testing.T) {
	repo := newTools(&fakeQuerier{})
	all, err := repo.GetToolsForToolsByName(context.Background(), ToolsProviderName)
	if err != nil {
		t.Fatalf("GetToolsForToolsByName(%q): %v", ToolsProviderName, err)
	}
	if len(all) != 1 || all[0].Function.Name != ToolSearch {
		t.Fatalf("descriptor = %+v", all)
	}
	if _, err := repo.GetToolsForToolsByName(context.Background(), "workspace"); err == nil {
		t.Error("the pre-revival unscoped name must not resolve; it would be a second name no allowlist entry or policy block addresses")
	}
}

// A declared native tool acts machine-locally by right of the declaration, but
// every call still passes the HITL gate. That gate is the wrapper the engine
// puts around PersistentRepo, so what this package must not do is refuse or
// approve anything itself: it exposes no Prechecker and no approval seam.
func TestUnit_Gate_ToolsetCarriesNoGateOfItsOwn(t *testing.T) {
	repo := newTools(&fakeQuerier{})
	if _, ok := repo.(taskengine.Prechecker); ok {
		t.Error("this toolset implements Prechecker; approval belongs to the HITL wrapper alone")
	}
	// A read-only toolset that never mutates the workspace still runs under the
	// wrapper's policy: it must not, for instance, short-circuit on a context
	// that carries no approval.
	if _, _, err := repo.Exec(context.Background(), time.Now(), map[string]any{"question": "x"}, false, &taskengine.ToolsCall{Name: ToolsProviderName, ToolName: ToolSearch}); err != nil {
		t.Fatalf("Exec: %v", err)
	}
}

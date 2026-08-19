package gointel

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
	repo, _ := newTestTools(t)

	got, err := repo.Supports(context.Background())
	if err != nil {
		t.Fatalf("Supports: %v", err)
	}
	if len(got) == 0 || got[0] != ToolsProviderName {
		t.Fatalf("Supports() = %v, want it to lead with %q", got, ToolsProviderName)
	}
	// The tools themselves are NOT prefixed: only toolset names reach the
	// allowlist, and go_describe & co. are the seeded HITL policy keys.
	for _, name := range toolNames {
		if strings.HasPrefix(name, "native-") {
			t.Errorf("tool name %q is prefixed; the namespace scopes toolsets, not tools", name)
		}
	}
}

// The descriptors are what an admitted toolset actually costs, so they are
// reachable under the registered key — the same key PersistentRepo dispatches
// on — and the pre-revival unscoped name resolves to nothing.
func TestUnit_Gate_DescriptorsAreReachableUnderTheRegisteredKey(t *testing.T) {
	repo, _ := newTestTools(t)
	ctx := context.Background()

	all, err := repo.GetToolsForToolsByName(ctx, ToolsProviderName)
	if err != nil {
		t.Fatalf("GetToolsForToolsByName(%q): %v", ToolsProviderName, err)
	}
	if len(all) != len(toolNames) {
		t.Fatalf("%d descriptors, want %d", len(all), len(toolNames))
	}

	if _, err := repo.GetToolsForToolsByName(ctx, "gointel"); err == nil {
		t.Error("the pre-revival unscoped name must not resolve; it would be a second name no allowlist entry or policy block addresses")
	}

	schemas, err := repo.GetSchemasForSupportedTools(ctx)
	if err != nil {
		t.Fatalf("GetSchemasForSupportedTools: %v", err)
	}
	if _, ok := schemas[ToolsProviderName]; !ok {
		t.Fatalf("no schema published under %q, got %v", ToolsProviderName, schemas)
	}
	if _, ok := schemas["gointel"]; ok {
		t.Error("a schema is still published under the pre-revival unscoped name")
	}
}

// A declared native tool acts machine-locally by right of the declaration, but
// every call still passes the HITL gate. That gate is the wrapper the engine
// puts around PersistentRepo, so what this package must not do is refuse or
// approve anything itself: it exposes no Prechecker and no approval seam.
func TestUnit_Gate_ToolsetCarriesNoGateOfItsOwn(t *testing.T) {
	repo, _ := newTestTools(t)
	if _, ok := repo.(taskengine.Prechecker); ok {
		t.Error("this toolset implements Prechecker; approval belongs to the HITL wrapper alone")
	}
	// Every tool is a pure read, so the toolset is allow-tier whole; it must
	// not short-circuit on a context that carries no approval of its own.
	if _, _, err := repo.Exec(context.Background(), time.Now(), map[string]any{"symbol": "shapes.Rect"}, false,
		&taskengine.ToolsCall{Name: ToolsProviderName, ToolName: ToolDefinition}); err != nil {
		t.Fatalf("Exec: %v", err)
	}
}

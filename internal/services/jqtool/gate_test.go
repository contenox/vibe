package jqtool_test

import (
	"context"
	"strings"
	"testing"

	"github.com/contenox/contenox/internal/kernel/taskengine"
	"github.com/contenox/contenox/internal/services/jqtool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// universe is what PersistentRepo.Supports hands the allowlist: this toolset's
// registered key alongside an unscoped operator registration and a declared MCP
// row.
func universe() []string {
	return []string{jqtool.ToolsProviderName, "local_fs", "decl-reviewer-github"}
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
	t.Parallel()

	// native- is a namespace, so a declared MCP source cannot mint this key.
	require.Truef(t, strings.HasPrefix(jqtool.ToolsProviderName, "native-"),
		"provider name %q dropped the native- namespace; a declared source could collide with it", jqtool.ToolsProviderName)

	assert.Truef(t, admits([]string{"*"}, jqtool.ToolsProviderName),
		"%q must be admitted by \"*\": the scope is a namespace, not a hidden exclusion", jqtool.ToolsProviderName)
	assert.True(t, admits([]string{"*"}, "local_fs"), "the wildcard no longer admits unscoped toolsets")
	assert.True(t, admits([]string{"*"}, "decl-reviewer-github"),
		"the wildcard must admit a declared MCP row too; \"*\" means everything")

	assert.Truef(t, admits([]string{jqtool.ToolsProviderName}, jqtool.ToolsProviderName),
		"%q is not admitted when a declaration names it exactly", jqtool.ToolsProviderName)
	assert.False(t, admits([]string{jqtool.ToolsProviderName}, "local_fs"),
		"a bare name granted more than exactly itself")
	assert.Falsef(t, admits([]string{"*", "!" + jqtool.ToolsProviderName}, jqtool.ToolsProviderName),
		"%q survives \"!\" under the wildcard; an operator cannot remove one set", jqtool.ToolsProviderName)
	assert.True(t, admits([]string{"*", "!" + jqtool.ToolsProviderName}, "local_fs"),
		"removing one set removed the others with it")
	assert.Falsef(t, admits([]string{jqtool.ToolsProviderName, "!" + jqtool.ToolsProviderName}, jqtool.ToolsProviderName),
		"%q survives its own denial entry", jqtool.ToolsProviderName)
	assert.Falsef(t, admits(nil, jqtool.ToolsProviderName),
		"%q is admitted with no allowlist at all", jqtool.ToolsProviderName)
	// The pre-revival unscoped name is not a second door into the same toolset.
	assert.False(t, admits([]string{"*"}, "jq"), "the pre-revival name is still in the universe")
}

// The gate keys on the toolset name, so the name the policy block, the HITL
// rules and the registration all use must be the one Supports reports.
func TestUnit_Gate_RegisteredNameIsTheNameSupportsReports(t *testing.T) {
	t.Parallel()

	got, err := jqtool.NewTools(t.TempDir()).Supports(context.Background())
	require.NoError(t, err)
	require.NotEmpty(t, got)
	assert.Equalf(t, jqtool.ToolsProviderName, got[0], "Supports() = %v, want it to lead with %q", got, jqtool.ToolsProviderName)

	// Scoped name ONLY: an allowlist addresses a SET, so the bare tool name must
	// not be a separate entry or "!native-jq" would fail to remove it. The engine
	// expands the toolset to its tools through GetToolsForToolsByName.
	assert.Lenf(t, got, 1, "Supports() = %v, want the scoped name alone", got)
	assert.NotContainsf(t, got, jqtool.ToolQuery,
		"Supports() leaks the bare tool %q; it becomes separately addressable and survives \"!%s\"", jqtool.ToolQuery, jqtool.ToolsProviderName)

	// The tool itself is NOT prefixed: only toolset names reach the allowlist,
	// and jq_query is the seeded HITL policy key.
	assert.Falsef(t, strings.HasPrefix(jqtool.ToolQuery, "native-"),
		"tool name %q is prefixed; the namespace scopes toolsets, not tools", jqtool.ToolQuery)
}

// Everything the host can remove with one "!" entry is everything Supports
// reports, and this feeds the REAL Supports() output through the allowlist, not
// a hand-built universe, so a regression that re-leaks the bare tool is caught
// here and not only in the model of it above.
func TestUnit_Gate_BangRemovesEverythingReportedBySupports(t *testing.T) {
	t.Parallel()
	got, err := jqtool.NewTools(t.TempDir()).Supports(context.Background())
	require.NoError(t, err)

	assert.Equal(t, []string{jqtool.ToolsProviderName},
		taskengine.ExportedApplyAllowlist([]string{"*"}, got),
		"\"*\" must admit the whole toolset under its one scoped name")

	removed := taskengine.ExportedApplyAllowlist([]string{"*", "!" + jqtool.ToolsProviderName}, got)
	assert.Emptyf(t, removed,
		"\"!%s\" left %v out of Supports() %v; a leaf escaped the removal", jqtool.ToolsProviderName, removed, got)

	assert.Equal(t, []string{jqtool.ToolsProviderName},
		taskengine.ExportedApplyAllowlist([]string{jqtool.ToolsProviderName}, got))
	assert.Empty(t, taskengine.ExportedApplyAllowlist(nil, got),
		"an empty allowlist grants nothing")
}

// The descriptor is what an admitted toolset actually costs, so it is reachable
// under the registered key — the same key PersistentRepo dispatches on.
func TestUnit_Gate_DescriptorIsReachableUnderTheRegisteredKey(t *testing.T) {
	t.Parallel()
	repo := jqtool.NewTools(t.TempDir())

	all, err := repo.GetToolsForToolsByName(context.Background(), jqtool.ToolsProviderName)
	require.NoError(t, err)
	require.Len(t, all, 1)
	assert.Equal(t, jqtool.ToolQuery, all[0].Function.Name)

	_, err = repo.GetToolsForToolsByName(context.Background(), "jq")
	require.Error(t, err, "the pre-revival unscoped name must not resolve; it would be a second name no allowlist entry or policy block addresses")

	docs, err := repo.GetSchemasForSupportedTools(context.Background())
	require.NoError(t, err)
	require.Contains(t, docs, jqtool.ToolsProviderName, "the contract is published under the registered key")
	require.NotContains(t, docs, "jq")
}

// A surface that registers this toolset under another scoped key gets a toolset
// that answers to it everywhere: Supports, the descriptor lookup, the published
// contract and the tools-policy block are the SAME name, or the gate admits a
// name whose definitions and policy nothing can reach.
func TestUnit_Gate_RenamedRegistrationIsAnsweredToEverywhere(t *testing.T) {
	t.Parallel()
	const alt = "native-jq-alt"
	repo := jqtool.NewToolsWith(t.TempDir(), alt, nil)
	ctx := context.Background()

	supported, err := repo.Supports(ctx)
	require.NoError(t, err)
	require.NotEmpty(t, supported)
	assert.Equal(t, alt, supported[0])

	all, err := repo.GetToolsForToolsByName(ctx, alt)
	require.NoError(t, err)
	require.Len(t, all, 1)

	_, err = repo.GetToolsForToolsByName(ctx, jqtool.ToolsProviderName)
	require.Error(t, err, "the default name must not resolve on a toolset registered under another key")

	docs, err := repo.GetSchemasForSupportedTools(ctx)
	require.NoError(t, err)
	require.Contains(t, docs, alt)
	require.NotContains(t, docs, jqtool.ToolsProviderName)
}

// A declared native tool acts machine-locally by right of the declaration, but
// every call still passes the HITL gate. That gate is the wrapper the engine
// puts around PersistentRepo, so what this package must not do is refuse or
// approve anything itself: it exposes no Prechecker and no approval seam.
func TestUnit_Gate_ToolsetCarriesNoGateOfItsOwn(t *testing.T) {
	t.Parallel()
	repo := jqtool.NewTools(t.TempDir())

	_, ok := repo.(taskengine.Prechecker)
	assert.False(t, ok, "this toolset implements Prechecker; approval belongs to the HITL wrapper alone")

	// A read-only toolset still runs under the wrapper's policy: it must not,
	// for instance, short-circuit on a context that carries no approval.
	res := mustExec(t, repo, map[string]any{"input": `{"a":1}`, "filter": ".a"})
	assert.Equal(t, []string{`1`}, values(res))
}

// The description is the only place the model learns that this toolset's reads
// are machine-local — local_fs routes its reads through the client, jq does not
// — so the boundary must be stated where it is paid for, on every turn.
func TestUnit_Gate_DescriptionStatesTheMachineLocalRead(t *testing.T) {
	t.Parallel()

	all, err := jqtool.NewTools(t.TempDir()).GetToolsForToolsByName(context.Background(), jqtool.ToolsProviderName)
	require.NoError(t, err)
	desc := all[0].Function.Description
	for _, want := range []string{"AGENT HOST", "local_fs", "declaration", "`input`"} {
		assert.Containsf(t, desc, want, "the description does not state the machine-local read boundary:\n%s", desc)
	}
}

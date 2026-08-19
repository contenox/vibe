package echotool

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/contenox/contenox/internal/kernel/taskengine"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
	t.Parallel()

	// native- is a namespace, so a declared MCP source cannot mint this key.
	require.Truef(t, strings.HasPrefix(ToolsProviderName, "native-"),
		"provider name %q dropped the native- namespace; a declared source could collide with it", ToolsProviderName)

	assert.Truef(t, admits([]string{"*"}, ToolsProviderName),
		"%q must be admitted by \"*\": the scope is a namespace, not a hidden exclusion", ToolsProviderName)
	assert.True(t, admits([]string{"*"}, "local_fs"), "the wildcard no longer admits unscoped toolsets")
	assert.True(t, admits([]string{"*"}, "decl-reviewer-github"),
		"the wildcard must admit a declared MCP row too; \"*\" means everything")

	assert.Truef(t, admits([]string{ToolsProviderName}, ToolsProviderName),
		"%q is not admitted when a declaration names it exactly", ToolsProviderName)
	assert.False(t, admits([]string{ToolsProviderName}, "local_fs"),
		"a bare name granted more than exactly itself")
	assert.Falsef(t, admits([]string{"*", "!" + ToolsProviderName}, ToolsProviderName),
		"%q survives \"!\" under the wildcard; an operator cannot remove one set", ToolsProviderName)
	assert.True(t, admits([]string{"*", "!" + ToolsProviderName}, "local_fs"),
		"removing one set removed the others with it")
	assert.Falsef(t, admits([]string{ToolsProviderName, "!" + ToolsProviderName}, ToolsProviderName),
		"%q survives its own denial entry", ToolsProviderName)
	assert.Falsef(t, admits(nil, ToolsProviderName),
		"%q is admitted with no allowlist at all", ToolsProviderName)
	// The pre-revival unscoped key is not a second door into the same toolset.
	assert.NotContains(t, universe(), ToolEcho, "the unscoped registration key is back in the universe")
}

// The unscoped tool name would be its own allowlist entry, addressable and
// removable apart from the toolset it belongs to; the scoped key is the one
// entry that carries the whole set.
func TestUnit_Gate_TheUnscopedNameWouldBeSeparatelyAddressable(t *testing.T) {
	t.Parallel()

	assert.Equal(t, []string{ToolEcho},
		taskengine.ExportedApplyAllowlist([]string{"*", "!" + ToolsProviderName}, []string{ToolEcho}),
		"a universe carrying the bare tool name survives \"!%s\"; that is why Supports must report the toolset key alone", ToolsProviderName)
	assert.Empty(t, taskengine.ExportedApplyAllowlist([]string{"*", "!" + ToolsProviderName}, []string{ToolsProviderName}),
		"the scoped key is the one entry that carries the whole set, so removing it removes the tools")
}

// The gate keys on the toolset name, so the name the policy block, the HITL
// rules and the registration all use must be the one Supports reports.
func TestUnit_Gate_RegisteredNameIsTheNameSupportsReports(t *testing.T) {
	t.Parallel()

	got, err := NewTools().Supports(context.Background())
	require.NoError(t, err)
	require.NotEmpty(t, got)
	assert.Equalf(t, ToolsProviderName, got[0], "Supports() = %v, want it to lead with %q", got, ToolsProviderName)
	// Scoped-name only: the unscoped tool name must not leak into Supports(), or a
	// MultiRepo universe carries it as its own entry and "!native-echo" no longer
	// removes it. See TestUnit_Gate_SupportsReportsScopedNameOnly.
	assert.NotContainsf(t, got, ToolEcho,
		"Supports() leaks the unscoped tool name %q; it becomes separately addressable", ToolEcho)

	// The tool itself is NOT prefixed: only toolset names reach the allowlist,
	// and echo is the seeded HITL policy key.
	assert.Falsef(t, strings.HasPrefix(ToolEcho, "native-"),
		"tool name %q is prefixed; the namespace scopes toolsets, not tools", ToolEcho)
	// The registration key and the tool name were the same word before the
	// revival; they must not be again, or the toolset an allowlist addresses and
	// the tool it contains are indistinguishable.
	assert.NotEqual(t, ToolEcho, ToolsProviderName)
}

// TestUnit_Gate_SupportsReportsScopedNameOnly pins the scoped-name-only
// invariant against the real allowlist. A MultiRepo folds every repo's Supports()
// into the universe applyAllowlist filters, so an unscoped `echo` reported here
// would be its own allowlist entry: an operator writing "!native-echo" would not
// remove it. The scoped key is the one name that addresses the whole set.
func TestUnit_Gate_SupportsReportsScopedNameOnly(t *testing.T) {
	t.Parallel()

	got, err := NewTools().Supports(context.Background())
	require.NoError(t, err)
	require.Equalf(t, []string{ToolsProviderName}, got,
		"Supports() must report the scoped toolset key alone, got %v", got)

	assert.Equal(t, []string{ToolsProviderName},
		taskengine.ExportedApplyAllowlist([]string{"*"}, got),
		"\"*\" must admit the whole toolset under its one scoped name")

	removed := taskengine.ExportedApplyAllowlist([]string{"*", "!" + ToolsProviderName}, got)
	assert.Emptyf(t, removed,
		"\"!%s\" left %v behind; a leaf escaped the removal", ToolsProviderName, removed)

	assert.Equal(t, []string{ToolsProviderName},
		taskengine.ExportedApplyAllowlist([]string{ToolsProviderName}, got),
		"an exact declaration must admit the scoped toolset")

	assert.Empty(t, taskengine.ExportedApplyAllowlist(nil, got),
		"an empty allowlist grants nothing")

	// GetToolsForToolsByName still answers under the scoped key, so a declared
	// toolset renders its descriptor; the tool name absent from Supports() has
	// not made the descriptor unreachable.
	all, err := NewTools().GetToolsForToolsByName(context.Background(), ToolsProviderName)
	require.NoError(t, err)
	require.Len(t, all, 1)
	assert.Equal(t, ToolEcho, all[0].Function.Name)
}

// The descriptor is what an admitted toolset actually costs, so it is reachable
// under the registered key — the same key PersistentRepo dispatches on.
func TestUnit_Gate_DescriptorIsReachableUnderTheRegisteredKey(t *testing.T) {
	t.Parallel()
	repo := NewTools()

	all, err := repo.GetToolsForToolsByName(context.Background(), ToolsProviderName)
	require.NoError(t, err)
	require.Len(t, all, 1)
	assert.Equal(t, ToolEcho, all[0].Function.Name)

	_, err = repo.GetToolsForToolsByName(context.Background(), "native-echoes")
	require.Error(t, err, "a name this toolset is not registered under must not resolve")

	docs, err := repo.GetSchemasForSupportedTools(context.Background())
	require.NoError(t, err)
	require.Contains(t, docs, ToolsProviderName, "the contract is published under the registered key")
	require.NotContains(t, docs, ToolEcho)
}

// A surface that registers this toolset under another scoped key gets a toolset
// that answers to it everywhere: Supports, the descriptor lookup, the published
// contract and the tools-policy block are the SAME name, or the gate admits a
// name whose definitions and policy nothing can reach.
func TestUnit_Gate_RenamedRegistrationIsAnsweredToEverywhere(t *testing.T) {
	t.Parallel()
	const alt = "native-echo-alt"
	repo := NewToolsWith(alt)
	ctx := context.Background()

	supported, err := repo.Supports(ctx)
	require.NoError(t, err)
	require.NotEmpty(t, supported)
	assert.Equal(t, alt, supported[0])

	all, err := repo.GetToolsForToolsByName(ctx, alt)
	require.NoError(t, err)
	require.Len(t, all, 1)

	_, err = repo.GetToolsForToolsByName(ctx, ToolsProviderName)
	require.Error(t, err, "the default name must not resolve on a toolset registered under another key")

	docs, err := repo.GetSchemasForSupportedTools(ctx)
	require.NoError(t, err)
	require.Contains(t, docs, alt)
	require.NotContains(t, docs, ToolsProviderName)

	// The policy block follows the registration key too.
	ctx = taskengine.WithToolsArgs(ctx, alt, map[string]string{policyMaxEchoBytes: "64"})
	out, _, err := repo.Exec(ctx, time.Now(), strings.Repeat("x", 300), false,
		&taskengine.ToolsCall{Name: alt, ToolName: ToolEcho})
	require.NoError(t, err)
	assert.Contains(t, out.(string), "not echoed", "the renamed toolset does not read its own policy block")
}

// A declared native tool acts machine-locally by right of the declaration, but
// every call still passes the HITL gate. That gate is the wrapper the engine
// puts around PersistentRepo, so what this package must not do is refuse or
// approve anything itself: it exposes no Prechecker and no approval seam.
func TestUnit_Gate_ToolsetCarriesNoGateOfItsOwn(t *testing.T) {
	t.Parallel()
	repo := NewTools()

	_, ok := repo.(taskengine.Prechecker)
	assert.False(t, ok, "this toolset implements Prechecker; approval belongs to the HITL wrapper alone")

	// It must not short-circuit on a context that carries no approval either.
	out, dt, err := repo.Exec(context.Background(), time.Now(), map[string]any{"input": "hi"}, false,
		&taskengine.ToolsCall{Name: ToolsProviderName, ToolName: ToolEcho})
	require.NoError(t, err)
	assert.Equal(t, taskengine.DataTypeString, dt)
	assert.Equal(t, "hi", out)
}

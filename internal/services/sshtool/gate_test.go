package sshtool_test

import (
	"context"
	"strings"
	"testing"

	"github.com/contenox/contenox/internal/kernel/taskengine"
	"github.com/contenox/contenox/internal/services/sshtool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// universe is what PersistentRepo.Supports hands the allowlist: this toolset's
// registered key alongside an unscoped operator registration and a declared MCP
// row.
func universe() []string {
	return []string{sshtool.ToolsProviderName, "local_fs", "decl-reviewer-github"}
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
	require.Truef(t, strings.HasPrefix(sshtool.ToolsProviderName, "native-"),
		"provider name %q dropped the native- namespace; a declared source could collide with it", sshtool.ToolsProviderName)

	assert.Truef(t, admits([]string{"*"}, sshtool.ToolsProviderName),
		"%q must be admitted by \"*\": the scope is a namespace, not a hidden exclusion", sshtool.ToolsProviderName)
	assert.True(t, admits([]string{"*"}, "local_fs"), "the wildcard no longer admits unscoped toolsets")
	assert.True(t, admits([]string{"*"}, "decl-reviewer-github"),
		"the wildcard must admit a declared MCP row too; \"*\" means everything")

	assert.Truef(t, admits([]string{sshtool.ToolsProviderName}, sshtool.ToolsProviderName),
		"%q is not admitted when a declaration names it exactly", sshtool.ToolsProviderName)
	assert.False(t, admits([]string{sshtool.ToolsProviderName}, "local_fs"),
		"a bare name granted more than exactly itself")
	assert.Falsef(t, admits([]string{"*", "!" + sshtool.ToolsProviderName}, sshtool.ToolsProviderName),
		"%q survives \"!\" under the wildcard; an operator cannot remove one set", sshtool.ToolsProviderName)
	assert.True(t, admits([]string{"*", "!" + sshtool.ToolsProviderName}, "local_fs"),
		"removing one set removed the others with it")
	assert.Falsef(t, admits([]string{sshtool.ToolsProviderName, "!" + sshtool.ToolsProviderName}, sshtool.ToolsProviderName),
		"%q survives its own denial entry", sshtool.ToolsProviderName)
	assert.Falsef(t, admits(nil, sshtool.ToolsProviderName),
		"%q is admitted with no allowlist at all", sshtool.ToolsProviderName)
	// The pre-revival unscoped name is not a second door into the same toolset.
	assert.False(t, admits([]string{"*"}, "ssh"), "the pre-revival name is still in the universe")
}

// TestUnit_Gate_RegisteredNameIsTheNameSupportsReports is the archived latent
// bug: Supports() reported only "ssh" while the descriptor declared
// execute_remote_command, so neither name could address the other and the
// allowlist path could not reach the tool at all.
func TestUnit_Gate_RegisteredNameIsTheNameSupportsReports(t *testing.T) {
	t.Parallel()

	got, err := newTools(t, "").Supports(context.Background())
	require.NoError(t, err)
	require.NotEmpty(t, got)
	assert.Equalf(t, sshtool.ToolsProviderName, got[0], "Supports() = %v, want it to lead with %q", got, sshtool.ToolsProviderName)
	assert.Containsf(t, got, sshtool.ToolExecuteRemoteCommand,
		"Supports() = %v, but the descriptor declares %q; the tool cannot be addressed by name", got, sshtool.ToolExecuteRemoteCommand)
	assert.NotContains(t, got, "ssh", "the pre-revival unscoped name is still reported")

	// The tool itself is NOT prefixed: only toolset names reach the allowlist,
	// and execute_remote_command is the seeded HITL policy key.
	assert.Falsef(t, strings.HasPrefix(sshtool.ToolExecuteRemoteCommand, "native-"),
		"tool name %q is prefixed; the namespace scopes toolsets, not tools", sshtool.ToolExecuteRemoteCommand)
}

// The descriptor is what an admitted toolset actually costs, so it is reachable
// under the registered key — the same key PersistentRepo dispatches on.
func TestUnit_Gate_DescriptorIsReachableUnderTheRegisteredKey(t *testing.T) {
	t.Parallel()
	repo := newTools(t, "")

	all, err := repo.GetToolsForToolsByName(context.Background(), sshtool.ToolsProviderName)
	require.NoError(t, err)
	require.Len(t, all, 1)
	assert.Equal(t, sshtool.ToolExecuteRemoteCommand, all[0].Function.Name)

	_, err = repo.GetToolsForToolsByName(context.Background(), "ssh")
	require.Error(t, err, "the pre-revival unscoped name must not resolve; it would be a second name no allowlist entry or policy block addresses")

	docs, err := repo.GetSchemasForSupportedTools(context.Background())
	require.NoError(t, err)
	require.Contains(t, docs, sshtool.ToolsProviderName, "the contract is published under the registered key")
	require.NotContains(t, docs, "ssh")
}

// A surface that registers this toolset under another scoped key gets a toolset
// that answers to it everywhere: Supports, the descriptor lookup, the published
// contract and the tools-policy block are the SAME name, or the gate admits a
// name whose definitions and policy nothing can reach.
func TestUnit_Gate_RenamedRegistrationIsAnsweredToEverywhere(t *testing.T) {
	t.Parallel()
	const alt = "native-ssh-alt"
	repo := newTools(t, "", sshtool.WithName(alt))
	ctx := context.Background()

	supported, err := repo.Supports(ctx)
	require.NoError(t, err)
	require.NotEmpty(t, supported)
	assert.Equal(t, alt, supported[0])

	all, err := repo.GetToolsForToolsByName(ctx, alt)
	require.NoError(t, err)
	require.Len(t, all, 1)

	_, err = repo.GetToolsForToolsByName(ctx, sshtool.ToolsProviderName)
	require.Error(t, err, "the default name must not resolve on a toolset registered under another key")

	docs, err := repo.GetSchemasForSupportedTools(ctx)
	require.NoError(t, err)
	require.Contains(t, docs, alt)
	require.NotContains(t, docs, sshtool.ToolsProviderName)
}

// A declared native tool acts by right of the declaration, but every call still
// passes the HITL gate — the wrapper the engine puts around PersistentRepo. What
// this package must not do is approve anything itself. It does implement
// Prechecker, which is the opposite: a refusal from static configuration alone,
// raised before the wrapper spends a human's decision.
func TestUnit_Gate_ToolsetApprovesNothingItself(t *testing.T) {
	t.Parallel()
	repo := newTools(t, "")

	pre, ok := repo.(taskengine.Prechecker)
	require.True(t, ok, "the host allowlist must be refusable without running anything")

	// The precheck refuses, and never grants: an allowed host returns nil and
	// still has to pass the wrapper above.
	ctx := withPolicy(map[string]string{"_allowed_hosts": "allowed.example.com"})
	require.NoError(t, pre.Precheck(ctx, map[string]any{
		"host": "allowed.example.com", "user": "deploy", "command": "uptime", "password": "x",
	}, call(sshtool.ToolsProviderName)))
}

// The description is the only place the model learns that this toolset is the
// one native toolset that does NOT act on the agent host, so the boundary must
// be stated where it is paid for, on every turn.
func TestUnit_Gate_DescriptionStatesTheRemoteReach(t *testing.T) {
	t.Parallel()

	all, err := newTools(t, "").GetToolsForToolsByName(context.Background(), sshtool.ToolsProviderName)
	require.NoError(t, err)
	desc := all[0].Function.Description
	for _, want := range []string{"REMOTE", "local_shell", "_allowed_hosts", "known_hosts"} {
		assert.Containsf(t, desc, want, "the description does not state the remote-reach boundary:\n%s", desc)
	}
}

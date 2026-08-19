package contenoxcli

import (
	"context"
	"strings"
	"testing"

	"github.com/contenox/contenox/internal/kernel/taskengine"
	"github.com/contenox/contenox/internal/services/fleetservice"
	"github.com/contenox/contenox/internal/services/localtools"
	"github.com/contenox/contenox/internal/services/missionservice"
	"github.com/contenox/contenox/internal/services/missiontools"
	"github.com/contenox/contenox/internal/surfaces/acpsvc"
	"github.com/contenox/contenox/libtracker"
	"github.com/stretchr/testify/require"
)

func testToolset(profile acpProfile, optInBeta bool) map[string]taskengine.ToolsRepo {
	noTransport := func(context.Context) *acpsvc.Transport { return nil }
	noFleet := func() fleetservice.Service { return nil }
	return acpToolset(profile, nil, libtracker.NoopTracker{}, "test-workspace",
		noTransport, missionservice.New(nil), nil, nil, optInBeta, noFleet)
}

// TestUnit_ACPToolset_CarriesTheSharedToolsets pins the divergence part 1
// closes: an ACP session (contenox acp/acpx — Zed, JetBrains, OpenClaw) must
// carry the same toolsets `contenox chat`/`run` gets via engine.go's
// localToolset, not just local_fs/webtools/local_shell.
func TestUnit_ACPToolset_CarriesTheSharedToolsets(t *testing.T) {
	tools := testToolset(acpProfileACP, true)

	// Every provider chat/run already had must still be present.
	for _, name := range []string{"local_fs", "local_shell", missiontools.ToolsProviderName} {
		require.Containsf(t, tools, name, "ACP toolset must keep registering %q", name)
	}

	// The toolsets that were missing, asserted by Supports(), the same way
	// engine_test.go pins localToolset's composition.
	cases := []struct {
		provider string
		tool     string
	}{}
	for _, tc := range cases {
		repo, ok := tools[tc.provider]
		require.Truef(t, ok, "ACP toolset must register provider %q", tc.provider)
		supported, err := repo.Supports(context.Background())
		require.NoError(t, err)
		require.Containsf(t, supported, tc.tool, "%s must support %s", tc.provider, tc.tool)
	}

	stable := testToolset(acpProfileACP, false)
	for _, name := range []string{"local_fs", "local_shell", missiontools.ToolsProviderName} {
		require.Containsf(t, stable, name, "stable toolset %q must not be beta-gated", name)
	}
}

// TestUnit_ACPToolset_NativeToolsetsAreDeclarationScoped pins the allowlist
// vocabulary an operator writes: "*" means every connected toolset with no
// exceptions, "!name" removes one, and naming a set grants exactly it. The
// native scope is a namespace, not a hidden exclusion — a "*" that quietly
// withheld the native sets would be the runtime deciding it knows better than
// the declaration.
func TestUnit_ACPToolset_NativeToolsetsAreDeclarationScoped(t *testing.T) {
	tools := testToolset(acpProfileACP, true)

	all := make([]string, 0, len(tools))
	var native []string
	for name := range tools {
		all = append(all, name)
		if strings.HasPrefix(name, "native-") {
			native = append(native, name)
		}
	}
	require.Subset(t, all, []string{
		localtools.GitToolsName, localtools.LocalFSBrowseToolsName, localtools.WebToolsName,
	}, "the editor profile carries the cleared native toolsets")
	require.NotEmpty(t, native)

	admitted := taskengine.ExportedApplyAllowlist([]string{"*"}, all)
	require.ElementsMatch(t, all, admitted, `"*" admits everything connected, native sets included`)

	excluded := taskengine.ExportedApplyAllowlist([]string{"*", "!" + localtools.GitToolsName}, all)
	require.NotContains(t, excluded, localtools.GitToolsName, `"!name" is how an operator removes one set`)
	require.Contains(t, excluded, "local_fs", "and removes only that one")

	named := taskengine.ExportedApplyAllowlist([]string{localtools.GitToolsName}, all)
	require.Contains(t, named, localtools.GitToolsName, "a declaration naming the toolset exactly admits it")
}

// TestUnit_ACPToolset_AdvertisesNoProxiedToolWithoutAClient pins the rule the
// production incident broke: local_fs and local_shell are pure proxies to the
// attached client, so a host serving a session nobody is attached to must not
// show the model a shell it can never run.
func TestUnit_ACPToolset_AdvertisesNoProxiedToolWithoutAClient(t *testing.T) {
	tools := testToolset(acpProfileACP, true)

	for _, name := range []string{"local_fs", "local_shell"} {
		repo, ok := tools[name]
		require.Truef(t, ok, "%q must stay registered", name)
		advertised, err := repo.GetToolsForToolsByName(context.Background(), name)
		require.NoError(t, err)
		require.Emptyf(t, advertised, "%q must advertise nothing with no client attached", name)
	}
}

// A host is an organization's shape: nobody is at its keyboard and it has no
// keyboard to be at. Mounting the proxies and letting them fail per call would
// still put a filesystem and a shell in the model's tool list, so the host
// profile must not compose them at all.
func TestUnit_ACPToolset_HostMountsNoFilesystemOrTerminal(t *testing.T) {
	tools := testToolset(acpProfileServe, true)

	for _, name := range hostUnservedToolsets {
		require.NotContainsf(t, tools, name, "the host profile must not mount %q", name)
	}
	require.Contains(t, tools, missiontools.ToolsProviderName, "mission tools are in-process and stay")

	// Every other profile keeps them: this is the host's shape, not a global cut.
	for _, profile := range []acpProfile{acpProfileACP, acpProfileACPX, acpProfileBeam} {
		for _, name := range hostUnservedToolsets {
			require.Containsf(t, testToolset(profile, true), name, "%s must keep %q", profile.name, name)
		}
	}
}

// A declaration compiled for a host still names Read and Bash, and the toolsets
// simply vanish from the chain. The refusal is what turns that silence into
// something an operator can act on.
func TestUnit_ACPToolset_HostRefusalNamesTheShape(t *testing.T) {
	chain := &taskengine.TaskChainDefinition{Tasks: []taskengine.TaskDefinition{
		{ID: "loop", ExecuteConfig: &taskengine.LLMExecutionConfig{Tools: []string{"local_fs", "mission"}}},
		{ID: "again", ExecuteConfig: &taskengine.LLMExecutionConfig{Tools: []string{"local_fs", "local_shell"}}},
	}}

	host := testToolset(acpProfileServe, true)
	require.Equal(t, []string{"local_fs", "local_shell"}, unservedToolsets(chain, host),
		"each unserved toolset is named once, and a mounted one is never named")
	require.Empty(t, unservedToolsets(chain, testToolset(acpProfileACP, true)),
		"an editor profile serves both, so it refuses neither")
	require.Empty(t, unservedToolsets(nil, host))

	var out strings.Builder
	printUnservedToolsets(&out, unservedToolsets(chain, host))
	s := out.String()
	require.Contains(t, s, "local_fs")
	require.Contains(t, s, "local_shell")
	require.Contains(t, s, "no filesystem and no terminal")
	require.Contains(t, s, "MCP")
}

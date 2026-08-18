package acpsvc

import (
	"context"
	"testing"
	"time"

	"github.com/contenox/contenox/internal/kernel/taskengine"
	"github.com/contenox/contenox/internal/services/localtools"
	libacp "github.com/contenox/contenox/libacp"
	"github.com/stretchr/testify/require"
)

func mockTransportForCaps(caps libacp.ClientCapabilities) *Transport {
	t := &Transport{
		sessions:        make(map[libacp.SessionID]*sessionEntry),
		contenoxToACPID: make(map[string]libacp.SessionID),
	}
	t.clientCaps = caps
	return t
}

func gatedProxiedToolsets(tr *Transport) map[string]taskengine.ToolsRepo {
	resolve := func(context.Context) *Transport { return tr }
	return map[string]taskengine.ToolsRepo{
		localtools.LocalFSToolsName: ClientBackedToolset(localtools.NewLocalFSToolsWith(
			"", nil, NewACPFileIO(resolve), localtools.LocalFSToolsName, NewACPCwdResolver(resolve),
		), resolve),
		localtools.LocalExecToolsName: ClientBackedToolset(localtools.NewLocalExecToolsWith(
			NewACPCommandRunnerWithShell(resolve, localtools.DetectPlatformShell()),
		), resolve),
	}
}

func advertised(t *testing.T, repos map[string]taskengine.ToolsRepo, name string) []string {
	t.Helper()
	tools, err := repos[name].GetToolsForToolsByName(context.Background(), name)
	require.NoError(t, err)
	names := make([]string, 0, len(tools))
	for _, tool := range tools {
		names = append(names, tool.Function.Name)
	}
	return names
}

func TestUnit_ClientBackedToolset_ClientAdvertisingNothingSeesNeitherTool(t *testing.T) {
	repos := gatedProxiedToolsets(mockTransportForCaps(libacp.ClientCapabilities{}))

	require.Empty(t, advertised(t, repos, localtools.LocalFSToolsName))
	require.Empty(t, advertised(t, repos, localtools.LocalExecToolsName))
}

func TestUnit_ClientBackedToolset_FSOnlyClientSeesNoShell(t *testing.T) {
	repos := gatedProxiedToolsets(mockTransportForCaps(libacp.ClientCapabilities{
		FS: libacp.FileSystemCapabilities{ReadTextFile: true, WriteTextFile: true},
	}))

	require.ElementsMatch(t,
		[]string{"read_file", "read_file_range", "write_file", "edit_file", "sed"},
		advertised(t, repos, localtools.LocalFSToolsName))
	require.Empty(t, advertised(t, repos, localtools.LocalExecToolsName))
}

func TestUnit_ClientBackedToolset_TerminalAndFSClientSeesBoth(t *testing.T) {
	repos := gatedProxiedToolsets(mockTransportForCaps(libacp.ClientCapabilities{
		Terminal: true,
		FS:       libacp.FileSystemCapabilities{ReadTextFile: true, WriteTextFile: true},
	}))

	require.Contains(t, advertised(t, repos, localtools.LocalFSToolsName), "write_file")
	require.Equal(t, []string{localtools.LocalExecToolsName}, advertised(t, repos, localtools.LocalExecToolsName))
}

// A read-only client can serve the read tools and no mutation: every local_fs
// write re-reads the current file through the same client first.
func TestUnit_ClientBackedToolset_ReadOnlyClientSeesTheReadHalfOnly(t *testing.T) {
	repos := gatedProxiedToolsets(mockTransportForCaps(libacp.ClientCapabilities{
		FS: libacp.FileSystemCapabilities{ReadTextFile: true},
	}))

	require.ElementsMatch(t, []string{"read_file", "read_file_range"},
		advertised(t, repos, localtools.LocalFSToolsName))
}

func TestUnit_ClientBackedToolset_WriteOnlyClientSeesNoMutation(t *testing.T) {
	repos := gatedProxiedToolsets(mockTransportForCaps(libacp.ClientCapabilities{
		FS: libacp.FileSystemCapabilities{WriteTextFile: true},
	}))

	require.Empty(t, advertised(t, repos, localtools.LocalFSToolsName))
}

// A host serves sessions no client is attached to; nothing is advertised and the
// backings still refuse, rather than falling back to the host.
func TestUnit_ClientBackedToolset_NoAttachedClientAdvertisesNothing(t *testing.T) {
	repos := gatedProxiedToolsets(nil)

	require.Empty(t, advertised(t, repos, localtools.LocalFSToolsName))
	require.Empty(t, advertised(t, repos, localtools.LocalExecToolsName))
}

// A proxied toolset sits between the approval gate and the shell tool, so a call
// the shell will refuse must still be refused here — before a human is asked.
func TestUnit_ClientBackedToolset_ForwardsThePrecheck(t *testing.T) {
	repos := gatedProxiedToolsets(nil)

	pre, ok := repos[localtools.LocalExecToolsName].(taskengine.Prechecker)
	require.True(t, ok, "the proxied shell must keep the precheck seam its inner tool has")

	ctx := taskengine.WithToolsArgs(context.Background(), localtools.LocalExecToolsName,
		map[string]string{"_allowed_commands": "git,go"})
	err := pre.Precheck(ctx, nil, &taskengine.ToolsCall{
		Name:     localtools.LocalExecToolsName,
		ToolName: localtools.LocalExecToolsName,
		Args:     map[string]string{"command": "kubectl"},
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "kubectl")
}

func TestUnit_ClientBackedToolset_KeepsRegistrationAndExecRefusal(t *testing.T) {
	repos := gatedProxiedToolsets(nil)

	for _, name := range []string{localtools.LocalFSToolsName, localtools.LocalExecToolsName} {
		supported, err := repos[name].Supports(context.Background())
		require.NoError(t, err)
		require.Contains(t, supported, name, "%s must stay registered", name)
	}

	out, _, err := repos[localtools.LocalExecToolsName].Exec(context.Background(), time.Now(),
		map[string]any{"command": "git"}, false,
		&taskengine.ToolsCall{Name: localtools.LocalExecToolsName, ToolName: localtools.LocalExecToolsName})
	require.NoError(t, err)
	res, ok := out.(*localtools.LocalExecResult)
	require.True(t, ok)
	require.False(t, res.Success)
	require.Contains(t, res.Error, localtools.ErrNoTerminal.Error())
}

func TestUnit_FilterToolsForCaps_LeavesAToolsetItDoesNotProxy(t *testing.T) {
	tools := []taskengine.Tool{{Function: taskengine.FunctionTool{Name: "mission_report"}}}

	require.Equal(t, tools, filterToolsForCaps("mission", tools, libacp.ClientCapabilities{}))
}

// TestUnit_RequiredClientCapability_MirrorsClientCanServe pins the pairing: a
// tool names a capability exactly when zero caps cannot serve it, and granting
// the named capability is exactly what makes clientCanServe true.
func TestUnit_RequiredClientCapability_MirrorsClientCanServe(t *testing.T) {
	capsFor := func(label string) libacp.ClientCapabilities {
		switch label {
		case "fs.readTextFile":
			return libacp.ClientCapabilities{FS: libacp.FileSystemCapabilities{ReadTextFile: true}}
		case "fs.readTextFile+fs.writeTextFile":
			return libacp.ClientCapabilities{FS: libacp.FileSystemCapabilities{ReadTextFile: true, WriteTextFile: true}}
		case "terminal":
			return libacp.ClientCapabilities{Terminal: true}
		}
		t.Fatalf("RequiredClientCapability returned an unknown label %q", label)
		return libacp.ClientCapabilities{}
	}
	cases := []struct{ toolset, tool string }{
		{localtools.LocalFSToolsName, "read_file"},
		{localtools.LocalFSToolsName, "read_file_range"},
		{localtools.LocalFSToolsName, "write_file"},
		{localtools.LocalFSToolsName, "edit_file"},
		{localtools.LocalFSToolsName, "sed"},
		{localtools.LocalExecToolsName, localtools.LocalExecToolsName},
		{"mission", "mission_report"},
	}
	for _, tc := range cases {
		label := RequiredClientCapability(tc.toolset, tc.tool)
		if label == "" {
			require.Truef(t, clientCanServe(tc.toolset, tc.tool, libacp.ClientCapabilities{}),
				"%s/%s names no capability, so zero caps must serve it", tc.toolset, tc.tool)
			continue
		}
		require.Falsef(t, clientCanServe(tc.toolset, tc.tool, libacp.ClientCapabilities{}),
			"%s/%s names %q, so zero caps must not serve it", tc.toolset, tc.tool, label)
		require.Truef(t, clientCanServe(tc.toolset, tc.tool, capsFor(label)),
			"%s/%s must be served by exactly the caps its label %q names", tc.toolset, tc.tool, label)
	}
}

// TestUnit_PotentialClientTools_ReportsTheUnfilteredListWithoutAClient pins the
// doctor seam: the potential list names every proxied tool while the advertised
// list stays capability-filtered.
func TestUnit_PotentialClientTools_ReportsTheUnfilteredListWithoutAClient(t *testing.T) {
	repos := gatedProxiedToolsets(nil)

	repo := repos[localtools.LocalFSToolsName]
	require.True(t, IsClientBacked(repo))
	potential, err := PotentialClientTools(context.Background(), repo, localtools.LocalFSToolsName)
	require.NoError(t, err)
	names := make([]string, 0, len(potential))
	for _, tool := range potential {
		names = append(names, tool.Function.Name)
	}
	require.ElementsMatch(t, []string{"read_file", "read_file_range", "write_file", "edit_file", "sed"}, names)
	require.Empty(t, advertised(t, repos, localtools.LocalFSToolsName),
		"the advertised list must stay capability-filtered")
}

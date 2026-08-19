package contenoxcli

import (
	"fmt"
	"io"
	"slices"

	"github.com/contenox/contenox/internal/kernel/taskengine"
	"github.com/contenox/contenox/internal/services/echotool"
	"github.com/contenox/contenox/internal/services/fleetservice"
	"github.com/contenox/contenox/internal/services/gointel"
	"github.com/contenox/contenox/internal/services/gojatool"
	"github.com/contenox/contenox/internal/services/hitlservice"
	"github.com/contenox/contenox/internal/services/jqtool"
	"github.com/contenox/contenox/internal/services/localtools"
	"github.com/contenox/contenox/internal/services/missionservice"
	"github.com/contenox/contenox/internal/services/missiontools"
	"github.com/contenox/contenox/internal/services/sshtool"
	"github.com/contenox/contenox/internal/surfaces/acpsvc"
	libdb "github.com/contenox/contenox/libdbexec"
	"github.com/contenox/contenox/libtracker"
)

// acpToolset is the CLI's full localToolset plus the ACP fs/shell wiring that
// routes through the live Transport instead of a fixed cwd, plus this profile's
// mission tools. Same tool names, so the seeded HITL policies gate it the same
// way. The host profile mounts neither fs nor shell: a host is an
// organization's shape, and every capability it has is an MCP server.
func acpToolset(
	profile acpProfile,
	db libdb.DBManager,
	tracker libtracker.ActivityTracker,
	workspaceID string,
	transportFn acpsvc.TransportResolver,
	missions missionservice.Service,
	acpHITL hitlservice.Service,
	bus missionservice.EventPublisher,
	optInBeta bool,
	// fleetFn late-binds the in-process fleet, which is built after this toolset.
	fleetFn func() fleetservice.Service,
) map[string]taskengine.ToolsRepo {
	cwdResolver := acpsvc.NewACPCwdResolver(transportFn)
	supervision := missionSupervision{missions: missions, hitl: acpHITL, db: db, tracker: tracker}
	tools := map[string]taskengine.ToolsRepo{
		// Inert without a mission id in session/new `_meta`, so an ordinary editor
		// session only ever sees the supervisor half.
		missiontools.ToolsProviderName: missiontools.New(missions,
			missiontools.WithAttentionAsker(missionAttentionAsker{
				hitl:     acpHITL,
				missions: missions,
				bus:      bus,
			}),
			missiontools.WithSupervision(supervision),
			missiontools.WithAttentionResolver(supervision),
			missiontools.WithSpawner(fleetSpawner{fleet: fleetFn}, missions, subagentDefaults(db)),
		),
	}
	if profile.host {
		return tools
	}
	tools[localtools.LocalFSToolsName] = acpsvc.ClientBackedToolset(localtools.NewLocalFSToolsWith(
		"",
		db,
		acpsvc.NewACPFileIO(transportFn),
		localtools.LocalFSToolsName,
		cwdResolver,
	), transportFn)
	tools[localtools.LocalExecToolsName] = acpsvc.ClientBackedToolset(localtools.NewLocalExecToolsWith(
		acpsvc.NewACPCommandRunnerWithShell(transportFn, localtools.DetectPlatformShell()),
	), transportFn)
	// The native-* toolsets, carried by every profile except the host above.
	// "native-" is a namespace, not a gate: a declaration's "*" admits them
	// like anything else, "!native-git" removes one, and naming one grants it. Each contains its own reach through internal/services/vfs the
	// way local_fs does; the ones that own a cwd take the session's, resolved per
	// call. None is client-backed: they run on the machine contenox runs on, not
	// through the editor's transport.
	tools[localtools.GitToolsName] = localtools.NewGitToolsWith("", localtools.GitToolsName, cwdResolver)
	tools[localtools.LocalFSBrowseToolsName] = localtools.NewLocalFSBrowseTools("", cwdResolver)
	tools[localtools.WebToolsName] = localtools.NewWebCaller(tracker)
	tools[jqtool.ToolsProviderName] = jqtool.NewToolsWith("", jqtool.ToolsProviderName, cwdResolver)
	tools[echotool.ToolsProviderName] = echotool.NewTools()
	// IdleTimeout below zero keeps the reaper goroutine unstarted, so this is safe
	// to build on the doctor roster's inert path; MaxRoots still bounds the cache.
	tools[gointel.ToolsProviderName] = gointel.NewTools(gointel.NewIndex(gointel.Config{
		CwdResolver: cwdResolver,
		IdleTimeout: -1,
	}))
	if goja, err := gojatool.New(gojatool.Config{}); err == nil {
		tools[gojatool.ScopedToolsProviderName] = goja
	}
	// ssh declines rather than mounts when no host-key store can be established;
	// an editor session without one simply does not carry the toolset.
	if ssh, err := sshtool.NewSSHTools(); err == nil {
		tools[sshtool.ToolsProviderName] = ssh
	}
	return tools
}

// hostUnservedToolsets are the toolsets a host will never mount, whatever a declaration asks for.
var hostUnservedToolsets = []string{localtools.LocalFSToolsName, localtools.LocalExecToolsName}

const hostUnservedToolsetRefusal = "this host serves no filesystem and no terminal — every capability is an MCP server; declare an MCP tool for it (contenox mcp add), or run this agent from `contenox beam` or an ACP editor"

// unservedToolsets names the toolsets chain asks for that mounted does not carry.
func unservedToolsets(chain *taskengine.TaskChainDefinition, mounted map[string]taskengine.ToolsRepo) []string {
	if chain == nil {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	for _, task := range chain.Tasks {
		if task.ExecuteConfig == nil {
			continue
		}
		for _, name := range task.ExecuteConfig.Tools {
			if seen[name] || mounted[name] != nil || !slices.Contains(hostUnservedToolsets, name) {
				continue
			}
			seen[name] = true
			out = append(out, name)
		}
	}
	return out
}

func printUnservedToolsets(w io.Writer, names []string) {
	for _, name := range names {
		fmt.Fprintf(w, "contenox serve: %q is declared but not served: %s\n", name, hostUnservedToolsetRefusal)
	}
}

package contenoxcli

import (
	"fmt"
	"io"
	"slices"

	"github.com/contenox/contenox/internal/kernel/taskengine"
	"github.com/contenox/contenox/internal/services/fleetservice"
	"github.com/contenox/contenox/internal/services/hitlservice"
	"github.com/contenox/contenox/internal/services/localtools"
	"github.com/contenox/contenox/internal/services/missionservice"
	"github.com/contenox/contenox/internal/services/missiontools"
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

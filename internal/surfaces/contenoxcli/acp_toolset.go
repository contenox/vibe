// acp_toolset.go composes the local tool providers an ACP session (contenox
// acp/acpx — Zed, JetBrains, OpenClaw) gets. It exists so the composition is
// assertable without a live ACP transport (acp_toolset_test.go), the same
// reason engine.go factors localToolset out of BuildEngine.
package contenoxcli

import (
	"github.com/contenox/contenox/internal/kernel/taskengine"
	"github.com/contenox/contenox/internal/services/fleetservice"
	"github.com/contenox/contenox/internal/services/gojatool"
	"github.com/contenox/contenox/internal/services/hitlservice"
	"github.com/contenox/contenox/internal/services/localtools"
	"github.com/contenox/contenox/internal/services/missionservice"
	"github.com/contenox/contenox/internal/services/missiontools"
	"github.com/contenox/contenox/internal/services/searchtool"
	"github.com/contenox/contenox/internal/surfaces/acpsvc"
	libdb "github.com/contenox/contenox/libdbexec"
	"github.com/contenox/contenox/libtracker"
)

// acpToolset is the CLI's full localToolset (engine.go) plus the ACP fs/shell
// wiring that routes through the live Transport instead of a fixed cwd, plus
// this profile's mission tools. Same construction, same tool names, so the
// seeded HITL policies (which already rule on workspace/goja) gate an ACP
// session exactly as they gate `contenox chat`/`run`. optInBeta gates goja
// registration exactly as localToolset does.
func acpToolset(
	db libdb.DBManager,
	tracker libtracker.ActivityTracker,
	gt *gojatool.Toolset,
	workspaceID string,
	transportFn func() *acpsvc.Transport,
	shellScrub func([]string) []string,
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
		"webtools": localtools.NewWebCaller(tracker),
		"local_fs": localtools.NewLocalFSToolsWith(
			"",
			db,
			acpsvc.NewACPFileIO(transportFn),
			localtools.LocalFSToolsName,
			cwdResolver,
		),
		"local_shell": localtools.NewLocalExecToolsWith(
			acpsvc.NewACPCommandRunnerWithScrub(transportFn, localtools.DetectPlatformShell(), shellScrub),
		),
		// The toolsets `contenox chat`/`run` get via localToolset and an ACP
		// session previously didn't: same construction, gated by the same
		// seeded policies (workspace/goja are allow-tier reads or pure
		// compute; git's four writes still approve).
		localtools.GitToolsName:      localtools.NewGitToolsWith("", localtools.GitToolsName, cwdResolver),
		searchtool.ToolsProviderName: newWorkspaceSearchTools(workspaceID),
		// Inert without a mission id in session/new `_meta`, so an ordinary editor session only ever sees the supervisor half.
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
	if optInBeta {
		tools[gojatool.ToolsProviderName] = gt
	}
	return tools
}

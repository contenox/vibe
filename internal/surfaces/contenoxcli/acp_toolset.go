package contenoxcli

import (
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
// mission tools. Same tool names, so the seeded HITL policies gate it the same way.
func acpToolset(
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
		"local_fs": localtools.NewLocalFSToolsWith(
			"",
			db,
			acpsvc.NewACPFileIO(transportFn),
			localtools.LocalFSToolsName,
			cwdResolver,
		),
		"local_shell": localtools.NewLocalExecToolsWith(
			acpsvc.NewACPCommandRunnerWithShell(transportFn, localtools.DetectPlatformShell()),
		),
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
	return tools
}

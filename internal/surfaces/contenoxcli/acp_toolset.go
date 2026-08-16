// acp_toolset.go composes the local tool providers an ACP session (contenox
// acp/acpx — Zed, JetBrains, OpenClaw) gets. It exists so the composition is
// assertable without a live ACP transport (acp_toolset_test.go), the same
// reason engine.go factors localToolset out of BuildEngine.
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

// acpToolset is the CLI's full localToolset (engine.go) plus the ACP fs/shell
// wiring that routes through the live Transport instead of a fixed cwd, plus
// this profile's mission tools. Same construction, same tool names, so the
// seeded HITL policies gate an ACP session exactly as they gate `contenox
// chat`/`run`.
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
		// The toolsets `contenox chat`/`run` get via localToolset and an ACP
		// session previously didn't: same construction, gated by the same
		// seeded policies (git's four writes still approve).
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
	return tools
}

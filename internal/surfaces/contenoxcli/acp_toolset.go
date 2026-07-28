// acp_toolset.go composes the local tool providers an ACP session (contenox
// acp/acpx — Zed, JetBrains, OpenClaw) gets. It exists so the composition is
// assertable without a live ACP transport (acp_toolset_test.go), the same
// reason engine.go factors localToolset out of BuildEngine.
package contenoxcli

import (
	"github.com/contenox/beam/internal/kernel/taskengine"
	"github.com/contenox/beam/internal/libbus"
	libdb "github.com/contenox/beam/internal/libdbexec"
	"github.com/contenox/beam/internal/libtracker"
	"github.com/contenox/beam/internal/services/gointel"
	"github.com/contenox/beam/internal/services/gojatool"
	"github.com/contenox/beam/internal/services/hitlservice"
	"github.com/contenox/beam/internal/services/jqtool"
	"github.com/contenox/beam/internal/services/localtools"
	"github.com/contenox/beam/internal/services/missionservice"
	"github.com/contenox/beam/internal/services/missiontools"
	"github.com/contenox/beam/internal/services/searchtool"
	"github.com/contenox/beam/internal/surfaces/acpsvc"
)

// acpToolset is the CLI's full localToolset (engine.go) plus the ACP fs/shell
// wiring that routes through the live Transport instead of a fixed cwd, plus
// this profile's mission tools. Same construction, same tool names, so the
// seeded HITL policies (which already rule on gointel/workspace/jq/goja) gate
// an ACP session exactly as they gate `contenox chat`/`run`.
func acpToolset(
	db libdb.DBManager,
	tracker libtracker.ActivityTracker,
	goIndex gointel.Index,
	gt *gojatool.Toolset,
	workspaceID string,
	transportFn func() *acpsvc.Transport,
	shellScrub func([]string) []string,
	missions missionservice.Service,
	acpHITL hitlservice.Service,
	bus libbus.Messenger,
) map[string]taskengine.ToolsRepo {
	cwdResolver := acpsvc.NewACPCwdResolver(transportFn)
	return map[string]taskengine.ToolsRepo{
		"echo":     localtools.NewEchoTools(),
		"print":    localtools.NewPrint(tracker),
		"webtools": localtools.NewWebCaller(tracker),
		"local_fs": localtools.NewLocalFSToolsWith(
			"",
			db,
			acpsvc.NewACPFileIO(transportFn),
			localtools.LocalFSToolsName,
			cwdResolver,
			// Same seam localToolset wires (engine.go): a write through the
			// ACP local_fs tool path invalidates gointel's snapshot
			// immediately, instead of waiting on the mtime-sweep backstop.
			localtools.WithOnFileMutated(func(absPath string) { goIndex.Invalidate(absPath) }),
		),
		"local_shell": localtools.NewLocalExecToolsWith(
			acpsvc.NewACPCommandRunnerWithScrub(transportFn, localtools.DetectPlatformShell(), shellScrub),
		),
		// The five toolsets `contenox chat`/`run` get via localToolset and an
		// ACP session previously didn't: same construction, gated by the
		// same seeded policies (gointel/workspace/jq/goja are allow-tier
		// reads or pure compute; git's four writes still approve).
		localtools.GitToolsName:      localtools.NewGitToolsWith("", localtools.GitToolsName, cwdResolver),
		gointel.ToolsProviderName:    gointel.NewTools(goIndex),
		jqtool.ToolsProviderName:     jqtool.NewToolsWith("", jqtool.ToolsProviderName, cwdResolver),
		searchtool.ToolsProviderName: newWorkspaceSearchTools(workspaceID),
		gojatool.ToolsProviderName:   gt,
		// Mission tools: inert without a mission id in session/new `_meta`, so
		// an ordinary editor session never sees them. The asker raises
		// mission_ask_attention as a durable ask over the same store
		// `contenox approvals` reads.
		missiontools.ToolsProviderName: missiontools.New(missions, missionAttentionAsker{
			hitl:     acpHITL,
			missions: missions,
			bus:      bus,
		}, missiontools.WithSupervision(
			missionSupervision{missions: missions, hitl: acpHITL},
			missionSupervision{missions: missions, hitl: acpHITL},
		)),
	}
}

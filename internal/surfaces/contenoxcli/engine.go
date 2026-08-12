package contenoxcli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/contenox/contenox/internal/kernel/enginesvc"
	"github.com/contenox/contenox/internal/kernel/taskengine"
	"github.com/contenox/contenox/internal/services/agentservice"
	"github.com/contenox/contenox/internal/services/eventlog"
	"github.com/contenox/contenox/internal/services/gointel"
	"github.com/contenox/contenox/internal/services/gojatool"
	"github.com/contenox/contenox/internal/services/hitlservice"
	"github.com/contenox/contenox/internal/services/jqtool"
	"github.com/contenox/contenox/internal/services/localtools"
	"github.com/contenox/contenox/internal/services/missionservice"
	"github.com/contenox/contenox/internal/services/missiontools"
	"github.com/contenox/contenox/internal/services/searchtool"
	"github.com/contenox/contenox/internal/services/setupcheck"
	"github.com/contenox/contenox/internal/services/vfs"
	"github.com/contenox/contenox/internal/store/runtimetypes"
	libbus "github.com/contenox/contenox/libbus"
	"github.com/contenox/contenox/libdbexec"
	"github.com/contenox/contenox/libtracker"
)

// ComputeReadiness builds the engine (a read-only backend sync, never a model completion) and returns the evaluated setup readiness.
func ComputeReadiness(ctx context.Context, db libdbexec.DBManager, opts chatOpts) (setupcheck.Result, error) {
	engine, err := BuildEngine(ctx, db, opts)
	if err != nil {
		return setupcheck.Result{}, err
	}
	defer engine.Stop()
	return setupcheck.EnrichResultWithOllamaProbe(ctx, engine.SetupCheck), nil
}

type Engine = enginesvc.Engine

func BuildEngine(ctx context.Context, db libdbexec.DBManager, opts chatOpts) (*Engine, error) {
	var tracker libtracker.ActivityTracker = libtracker.NoopTracker{}
	if opts.EffectiveTracing {
		// nil logger = the process default handler; keeps this file out of the log/slog import graph.
		tracker = libtracker.NewLogActivityTracker(nil)
	}
	if opts.EffectiveTracker != nil {
		tracker = opts.EffectiveTracker
	}

	reportErr, reportChange, end := tracker.Start(ctx, "build", "engine")
	defer end()

	// The index owns a reaper goroutine; it rides engine.Stop below, and the guard here covers error returns before that.
	goIndex := gointel.NewIndex(gointel.Config{
		AllowedDir: opts.EffectiveLocalExecAllowedDir,
		CwdResolver: func(context.Context) string {
			cwd, err := os.Getwd()
			if err != nil {
				return ""
			}
			return cwd
		},
	})
	// Construction loads and validates every script file, so a broken script is a startup error, not a silently skipped tool. Opt-in-beta gated: without it the scripts are never loaded.
	gojaScriptDir := filepath.Join(opts.ContenoxDir, "tools")
	if !opts.EffectiveOptInBeta {
		gojaScriptDir = ""
	}
	gt, err := gojatool.New(gojatool.Config{ScriptDir: gojaScriptDir})
	if err != nil {
		goIndex.Shutdown()
		reportErr(err)
		return nil, err
	}

	// One bus for this process: the mission tools below must publish on the same bus the engine runs on, or a resumed unit's report/mission_finish reaches nothing.
	bus := libbus.NewSQLite(db.WithoutTransaction())

	engineBuilt := false
	defer func() {
		if !engineBuilt {
			goIndex.Shutdown()
			gt.Shutdown()
			bus.Close()
		}
	}()

	// Process-global and consulted by every vfs.Contain call; must be registered before any file tool exists so an agent can never reach the config, database, or policies that govern it.
	if err := vfs.SetControlPlaneDenied(controlPlaneDirs(opts.ContenoxDir)...); err != nil {
		reportErr(err)
		return nil, fmt.Errorf("register control-plane denylist: %w", err)
	}

	workspaceID := ResolveWorkspaceID(opts.ContenoxDir)

	// Built here and injected rather than minted internally: the resume-on-verdict hook can only be registered once the engine exists.
	var hitlSvc hitlservice.Service
	if opts.EffectiveHITL {
		hitlSvc = opts.EffectiveHITLService
		if hitlSvc == nil {
			hitlSvc = newHITLService(opts.ContenoxDir, runtimetypes.New(db.WithoutTransaction()), tracker, "")
		}
	}

	// Both the publisher and the attention asker matter on the resume path: without them, a resumed run's remaining questions silently downgrade to a self-answered blocker nobody is notified of.
	trigHook := eventlog.NewTriggerHolder()
	missionPub := missionEventPublisher(ctx, db, bus, workspaceID, tracker, trigHook)
	missions := missionservice.New(db, missionservice.WithEventPublisher(missionPub))
	// Nil only when HITL is off, the same condition under which no resume hook is registered, so no resumed run ever meets a nil asker.
	var missionAsker missiontools.AttentionAsker
	if hitlSvc != nil {
		missionAsker = missionAttentionAsker{hitl: hitlSvc, missions: missions, bus: missionPub}
	}

	tools := localToolset(opts, db, tracker, goIndex, gt, missions, missionAsker)

	askApproval := opts.EffectiveAskApproval
	if askApproval == nil {
		askApproval = NewCLIAskApproval(os.Stderr)
	}

	readinessModel, readinessProvider := readinessDefaults(opts)

	reportChange("phase", "tools_prepared")
	engine, err := enginesvc.Build(ctx, db, enginesvc.Config{
		DefaultModel:             opts.EffectiveDefaultModel,
		DefaultProvider:          opts.EffectiveDefaultProvider,
		AltDefaultModel:          opts.EffectiveAltDefaultModel,
		AltDefaultProvider:       opts.EffectiveAltDefaultProvider,
		ReadinessDefaultModel:    readinessModel,
		ReadinessDefaultProvider: readinessProvider,
		ContextLength:            opts.EffectiveContext,
		NoDeleteModels:           opts.EffectiveNoDeleteModels,
		LocalTools:               tools,
		EnableHITL:               opts.EffectiveHITL,
		AskApproval:              askApproval,
		HITLService:              hitlSvc,
		Tracker:                  tracker,
		Tracing:                  opts.EffectiveTracing,
		SkipBackendCycle:         opts.EffectiveSkipBackendCycle,
		WorkspaceID:              workspaceID,
		HITLPolicySource:         hitlPolicySource(opts.ContenoxDir),
		TaskEventSink:            opts.EffectiveTaskEventSink,
		Bus:                      bus, // reuse the one bus the mission tools publish on
		// Closes the goja sandbox's construction cycle: host.tool needs the aggregate repo the sandbox is itself registered inside.
		OnToolsRepoReady: func(repo taskengine.ToolsRepo) {
			gt.SetHost(gojatool.HostFromRepo(repo))
		},
	})
	if err != nil {
		reportErr(err)
		return nil, err
	}
	// The engine now has the resolved model route workspace_search needs.
	bindWorkspaceSearch(tools, db, engine)
	// Same ordering for local_fs's audio seam: read_file on an audio file transcribes through the engine's chat path from here on.
	bindAudioTranscriber(tools, engine)
	trigHook.Set(buildInProcessTriggerHook(ctx, db, opts.ContenoxDir, workspaceID, engine, opts, os.Stderr))
	if hitlSvc != nil {
		hitlservice.SetResumeHook(hitlSvc, agentservice.ResumeHook(agentservice.Deps{
			Engine:      engine,
			DB:          db,
			WorkspaceID: workspaceID,
		}))
	}
	reportChange("phase", "enginesvc_built")
	// Rides the engine's chainable stop so the index's reaper joins on shutdown.
	engineBuilt = true
	oldStop := engine.Stop
	engine.Stop = func() {
		// Draining before teardown keeps at-least-once true: a firing claims its row before running, so an exit mid-firing would strand the claim otherwise.
		trigHook.Drain(eventlog.DefaultDrainTimeout)
		goIndex.Shutdown()
		// Refuses further executions and joins in-flight ones (bounded — a call may be parked on a human approval).
		gt.Shutdown()
		oldStop()
		// Ours to close: enginesvc closes only a bus it minted itself.
		bus.Close()
	}
	return engine, nil
}

func localToolset(opts chatOpts, db libdbexec.DBManager, tracker libtracker.ActivityTracker, goIndex gointel.Index, gojaTools *gojatool.Toolset, missions missionservice.Service, missionAsker missiontools.AttentionAsker) map[string]taskengine.ToolsRepo {
	tools := map[string]taskengine.ToolsRepo{
		"echo":     localtools.NewEchoTools(),
		"print":    localtools.NewPrint(tracker),
		"webtools": localtools.NewWebCaller(tracker),
		// Wires a successful write_file/sed/edit_file into gointel's Invalidate so a query right after an edit sees it without waiting on the mtime-sweep backstop.
		"local_fs": localtools.NewLocalFSToolsWith(opts.EffectiveLocalExecAllowedDir, db, nil, localtools.LocalFSToolsName, nil,
			localtools.WithOnFileMutated(func(absPath string) { goIndex.Invalidate(absPath) })),
		// Always registered, unlike local_shell: the seeded policies allow the six reads and hold the four writes at an approval.
		localtools.GitToolsName: localtools.NewGitTools(opts.EffectiveLocalExecAllowedDir),
		// Always registered: every gointel tool is a read, allowed whole by the seeded policies (revisit before any mutating op lands here).
		gointel.ToolsProviderName: gointel.NewTools(goIndex),
		// Always registered: jq_query reads a file read_file already reaches, writes nothing, reaches no network, and is deadline-bounded.
		jqtool.ToolsProviderName: jqtool.NewTools(opts.EffectiveLocalExecAllowedDir),
		// Always registered, unbound: completed by bindWorkspaceSearch below once the engine's embedding seam exists.
		searchtool.ToolsProviderName: newWorkspaceSearchTools(ResolveWorkspaceID(opts.ContenoxDir)),
		// Wired, not durable-only: this engine resumes suspended mission chains, and a resumed unit asks its remaining questions here.
		missiontools.ToolsProviderName: missiontools.New(missions, missionAsker),
	}
	if opts.EffectiveOptInBeta {
		// Registered only under opt-in-beta: with no scripts, goja carries only goja_eval, a pure compute sandbox with no ambient I/O.
		tools[gojatool.ToolsProviderName] = gojaTools
	}
	if opts.EffectiveEnableLocalExec {
		execOpts := []localtools.LocalExecOption{}
		if opts.EffectiveLocalExecAllowedDir != "" {
			execOpts = append(execOpts, localtools.WithLocalExecAllowedDir(opts.EffectiveLocalExecAllowedDir))
		}
		// SANDBOX_* scrub composition (see sandbox_scrub.go); db nil only in tests that register the toolset but never call Exec on it.
		if db != nil {
			if shellScrub, _, err := resolvedSandboxEnv(db, tracker, opts.WarnW); err != nil {
				if opts.WarnW != nil {
					fmt.Fprintf(opts.WarnW, "warning: sandbox env scrub unavailable, local_shell inherits the full environment: %v\n", err)
				}
			} else if shellScrub != nil {
				execOpts = append(execOpts, localtools.WithLocalExecScrubEnv(shellScrub))
			}
		}
		tools["local_shell"] = localtools.NewLocalExecTools(execOpts...)

		// A message for the operator, not telemetry: reaching here takes an explicit --auto.
		if !opts.EffectiveHITL && opts.EffectiveLocalExecAllowedDir == "" && opts.WarnW != nil {
			fmt.Fprint(opts.WarnW, "warning: --auto disabled the approval prompt and local_shell has no allowed dir — the agent may run any command, anywhere.\n"+
				"         scope it with: --local-exec-allowed-dir .\n")
		}
	}
	// Host-scoped providers last, never overriding a standard registration.
	for name, repo := range opts.EffectiveExtraTools {
		if _, exists := tools[name]; !exists {
			tools[name] = repo
		}
	}
	return tools
}

func readinessDefaults(opts chatOpts) (model, provider string) {
	if opts.EffectiveDefaultModel != opts.EffectiveConfiguredModel &&
		opts.EffectiveDefaultModel != "" &&
		opts.EffectiveDefaultModel != defaultModel {
		model = opts.EffectiveDefaultModel
	}
	if opts.EffectiveDefaultProvider != opts.EffectiveConfiguredProvider &&
		opts.EffectiveDefaultProvider != "" {
		provider = opts.EffectiveDefaultProvider
	}
	return model, provider
}

func hitlPolicySource(primaryDir string) hitlservice.PolicySource {
	dirs := []string{primaryDir}
	if home, err := os.UserHomeDir(); err == nil {
		dirs = append(dirs, filepath.Join(home, ".contenox"))
	}
	return hitlservice.NewFSPolicySource(dirs...)
}

func newHITLService(contenoxDir string, store runtimetypes.Store, tracker libtracker.ActivityTracker, fallbackPolicy string) hitlservice.Service {
	svc := hitlservice.NewWithDefaultPolicy(hitlPolicySource(contenoxDir), runtimetypes.LocalTenantID, store, tracker, fallbackPolicy)
	hitlservice.SetWorkspaceID(svc, ResolveWorkspaceID(contenoxDir))
	return svc
}

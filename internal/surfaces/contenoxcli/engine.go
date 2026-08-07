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

// ComputeReadiness builds the engine (a read-only backend sync, never a model
// completion) and returns the evaluated setup readiness — the shared path
// behind `contenox doctor` and the setup wizard's final check.
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
		// nil logger = the process default handler (stderr for the CLI,
		// beam.log for beam), keeping this file out of the log/slog import
		// graph: slog is the tracker's sink, not an API this file calls.
		tracker = libtracker.NewLogActivityTracker(nil)
	}
	if opts.EffectiveTracker != nil {
		tracker = opts.EffectiveTracker
	}

	reportErr, reportChange, end := tracker.Start(ctx, "build", "engine")
	defer end()

	// The index owns a reaper goroutine, so it needs a lifecycle: it rides
	// engine.Stop below, and the guard here covers error returns before that.
	// CwdResolver makes the index usable with no AllowedDir declared; when set,
	// AllowedDir still wins.
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
	// The goja sandbox: goja_eval plus one tool per operator-authored script in
	// $CONTENOX_DIR/tools. Construction loads and validates every script file,
	// so a broken script is a startup error naming the file — never a
	// silently skipped tool the operator believes exists. Without opt-in-beta
	// the scripts are not even loaded: the toolset stays unregistered
	// (localToolset), and an invisible feature must not fail startup.
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

	// One bus for this process, built here instead of letting enginesvc mint
	// one internally, so the mission tools below publish on the same bus the
	// engine runs on. A resumed unit's report and its mission_finish must reach
	// the report router and fleetservice's status teardown; a second, undrained
	// bus would reap nothing.
	bus := libbus.NewSQLite(db.WithoutTransaction())

	engineBuilt := false
	defer func() {
		if !engineBuilt {
			goIndex.Shutdown()
			gt.Shutdown()
			bus.Close()
		}
	}()

	// The control-plane denylist is process-global and consulted by every
	// vfs.Contain call, so it must be registered before any file tool exists.
	// Registering it here rather than per-surface is what makes the guarantee
	// unconditional: an agent can never reach the config, database, or
	// policies that govern it. This registration used to live in the retired
	// `serve` command, and nothing replaced it — until it returned, the
	// denylist was empty at runtime and the refusal never fired.
	if err := vfs.SetControlPlaneDenied(controlPlaneDirs(opts.ContenoxDir)...); err != nil {
		reportErr(err)
		return nil, fmt.Errorf("register control-plane denylist: %w", err)
	}

	workspaceID := ResolveWorkspaceID(opts.ContenoxDir)

	// Built here and injected, rather than letting enginesvc mint one
	// internally, because the resume-on-verdict hook can only be registered
	// once the engine exists.
	var hitlSvc hitlservice.Service
	if opts.EffectiveHITL {
		hitlSvc = newHITLService(opts.ContenoxDir, runtimetypes.New(db.WithoutTransaction()), tracker, "")
	}

	// The mission store this engine's tools write through, wired with the same
	// dual-write publisher every other mission producer uses, and — when a HITL
	// service exists — with the attention asker that turns a unit's question
	// into a durable ask. Both matter on the resume path: a run resumed here
	// (see SetResumeHook below) re-enters mission_ask_attention for every
	// question its batch had not reached yet, and a publisher-less,
	// asker-less provider silently downgrades each one to a self-answered
	// blocker report nobody is notified of.
	// Late-bound like the acp and mission-fire hosts: the holder joins the
	// publisher now and receives the in-process dispatch hook once the engine
	// below exists, so a resumed unit's events fire triggers here too instead
	// of waiting for the catch-up dispatcher.
	trigHook := eventlog.NewTriggerHolder()
	missionPub := missionEventPublisher(ctx, db, bus, workspaceID, tracker, trigHook)
	missions := missionservice.New(db, missionservice.WithEventPublisher(missionPub))
	// Nil only when HITL is off — the same condition under which no resume hook
	// is registered below, so no resumed run ever meets a nil asker on this
	// engine. Wiring one without a gate would be the --auto posture answering
	// its own questions.
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
		// Closes the goja sandbox's construction cycle: host.tool needs the
		// aggregate repo the sandbox is itself registered inside. enginesvc
		// hands back the HITL-wrapped repo here, so a script's tool call meets
		// the same envelope a model call would.
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
		// First: a firing claims its (trigger, nid) row before running the
		// chain, so a host that exits mid-firing strands the claim and the
		// catch-up dispatcher then skips it. Draining before teardown is what
		// keeps at-least-once true for the process that fired.
		trigHook.Drain(eventlog.DefaultDrainTimeout)
		goIndex.Shutdown()
		// Refuses further executions and joins in-flight ones (bounded — a
		// call may be parked on a human approval).
		gt.Shutdown()
		oldStop()
		// Ours to close: enginesvc closes only a bus it minted itself.
		bus.Close()
	}
	return engine, nil
}

// localToolset is the set of local tool providers every CLI-side engine gets.
// It is a function rather than an inline literal so what is registered — and,
// just as importantly, what is registered only when asked for — is assertable
// without standing up a whole engine (engine_test.go).
func localToolset(opts chatOpts, db libdbexec.DBManager, tracker libtracker.ActivityTracker, goIndex gointel.Index, gojaTools *gojatool.Toolset, missions missionservice.Service, missionAsker missiontools.AttentionAsker) map[string]taskengine.ToolsRepo {
	tools := map[string]taskengine.ToolsRepo{
		"echo":     localtools.NewEchoTools(),
		"print":    localtools.NewPrint(tracker),
		"webtools": localtools.NewWebCaller(tracker),
		// WithOnFileMutated wires a successful write_file/sed/edit_file straight
		// into gointel's Invalidate, so a query right after an agent edit sees it
		// without waiting on the mtime-sweep backstop. localtools never imports
		// gointel — this callback is the whole seam.
		"local_fs": localtools.NewLocalFSToolsWith(opts.EffectiveLocalExecAllowedDir, db, nil, localtools.LocalFSToolsName, nil,
			localtools.WithOnFileMutated(func(absPath string) { goIndex.Invalidate(absPath) })),
		// Always registered, unlike local_shell: reading the repository is the
		// point. What it may do is the envelope's call — the seeded policies
		// allow the six reads and hold the four writes at an approval.
		localtools.GitToolsName: localtools.NewGitTools(opts.EffectiveLocalExecAllowedDir),
		// Always registered: every gointel tool is a read, allowed whole by
		// the seeded policies (revisit before any mutating op lands here).
		gointel.ToolsProviderName: gointel.NewTools(goIndex),
		// Always registered: jq_query reads a file read_file already reaches,
		// writes nothing, reaches no network, and is deadline-bounded
		// including recursion (see internal/services/jqtool).
		jqtool.ToolsProviderName: jqtool.NewTools(opts.EffectiveLocalExecAllowedDir),
		// Always registered, unbound: it's a read returning citations from the
		// index `contenox index` built. Completed by bindWorkspaceSearch below
		// once the engine's embedding seam exists (see workspaceSearchRepo in
		// index_cmd.go).
		searchtool.ToolsProviderName: newWorkspaceSearchTools(ResolveWorkspaceID(opts.ContenoxDir)),
		// Wired, not durable-only: this engine resumes suspended mission
		// chains, and a resumed unit asks its remaining questions here. The
		// asker makes each one a durable ask over the store `contenox
		// approvals` reads; missions carries the event publisher that announces
		// its reports and its terminal status change. Built by BuildEngine —
		// see the invariant on missionAsker there.
		missiontools.ToolsProviderName: missiontools.New(missions, missionAsker),
	}
	if opts.EffectiveOptInBeta {
		// Registered only under opt-in-beta: with no $CONTENOX_DIR/tools, goja
		// carries only goja_eval, a pure compute sandbox with no ambient I/O —
		// its only reach out is host.tool, gated by this same envelope. Script
		// tools get no rule, so operator-authored code falls to default_action.
		tools[gojatool.ToolsProviderName] = gojaTools
	}
	if opts.EffectiveEnableLocalExec {
		execOpts := []localtools.LocalExecOption{}
		if opts.EffectiveLocalExecAllowedDir != "" {
			execOpts = append(execOpts, localtools.WithLocalExecAllowedDir(opts.EffectiveLocalExecAllowedDir))
		}
		// SANDBOX_* scrub composition (see sandbox_scrub.go); db nil only in
		// tests that register the toolset but never call Exec on it.
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

		// A message for the operator, not telemetry: reaching here takes an
		// explicit --auto, so the human approval gate was removed on purpose.
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

// readinessDefaults returns the model/provider to credit during setup
// preflight, crediting the effective value whenever a flag override made it
// differ from persisted config — so an explicit `--provider vertex-google`
// isn't blocked by a broken persisted default.
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

// hitlPolicySource builds the CLI's HITL policy lookup: the workspace .contenox
// dir first, then the user's ~/.contenox as fallback.
func hitlPolicySource(primaryDir string) hitlservice.PolicySource {
	dirs := []string{primaryDir}
	if home, err := os.UserHomeDir(); err == nil {
		dirs = append(dirs, filepath.Join(home, ".contenox"))
	}
	return hitlservice.NewFSPolicySource(dirs...)
}

// newHITLService is the CLI's one HITL service constructor. It derives the
// policy source AND the workspace from the same contenoxDir, so the evaluator
// reads the cli.hitl-policy-name row `contenox config set hitl-policy-name`
// writes for that project. Constructing hitlservice directly here would leave
// the workspace unbound, and the policy switch silently inert.
func newHITLService(contenoxDir string, store runtimetypes.Store, tracker libtracker.ActivityTracker, fallbackPolicy string) hitlservice.Service {
	svc := hitlservice.NewWithDefaultPolicy(hitlPolicySource(contenoxDir), runtimetypes.LocalTenantID, store, tracker, fallbackPolicy)
	hitlservice.SetWorkspaceID(svc, ResolveWorkspaceID(contenoxDir))
	return svc
}

package contenoxcli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/contenox/beam/internal/kernel/enginesvc"
	"github.com/contenox/beam/internal/kernel/taskengine"
	"github.com/contenox/beam/internal/libdbexec"
	"github.com/contenox/beam/internal/libtracker"
	"github.com/contenox/beam/internal/services/agentservice"
	"github.com/contenox/beam/internal/services/gointel"
	"github.com/contenox/beam/internal/services/gojatool"
	"github.com/contenox/beam/internal/services/hitlservice"
	"github.com/contenox/beam/internal/services/jqtool"
	"github.com/contenox/beam/internal/services/localtools"
	"github.com/contenox/beam/internal/services/missionservice"
	"github.com/contenox/beam/internal/services/missiontools"
	"github.com/contenox/beam/internal/services/searchtool"
	"github.com/contenox/beam/internal/services/setupcheck"
	"github.com/contenox/beam/internal/store/runtimetypes"
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
	// silently skipped tool the operator believes exists.
	gt, err := gojatool.New(gojatool.Config{ScriptDir: filepath.Join(opts.ContenoxDir, "tools")})
	if err != nil {
		goIndex.Shutdown()
		reportErr(err)
		return nil, err
	}

	engineBuilt := false
	defer func() {
		if !engineBuilt {
			goIndex.Shutdown()
			gt.Shutdown()
		}
	}()

	tools := localToolset(opts, db, tracker, goIndex, gt)

	askApproval := opts.EffectiveAskApproval
	if askApproval == nil {
		askApproval = NewCLIAskApproval(os.Stderr)
	}

	readinessModel, readinessProvider := readinessDefaults(opts)

	// Built here and injected, rather than letting enginesvc mint one
	// internally, because the resume-on-verdict hook can only be registered
	// once the engine exists.
	var hitlSvc hitlservice.Service
	if opts.EffectiveHITL {
		hitlSvc = hitlservice.NewWithDefaultPolicy(hitlPolicySource(opts.ContenoxDir), runtimetypes.LocalTenantID, runtimetypes.New(db.WithoutTransaction()), tracker, "")
	}

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
		WorkspaceID:              ResolveWorkspaceID(opts.ContenoxDir),
		HITLPolicySource:         hitlPolicySource(opts.ContenoxDir),
		TaskEventSink:            opts.EffectiveTaskEventSink,
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
	if hitlSvc != nil {
		hitlservice.SetResumeHook(hitlSvc, agentservice.ResumeHook(agentservice.Deps{
			Engine:      engine,
			DB:          db,
			WorkspaceID: ResolveWorkspaceID(opts.ContenoxDir),
		}))
	}
	reportChange("phase", "enginesvc_built")
	// Rides the engine's chainable stop so the index's reaper joins on shutdown.
	engineBuilt = true
	oldStop := engine.Stop
	engine.Stop = func() {
		goIndex.Shutdown()
		// Refuses further executions and joins in-flight ones (bounded — a
		// call may be parked on a human approval).
		gt.Shutdown()
		oldStop()
	}
	return engine, nil
}

// localToolset is the set of local tool providers every CLI-side engine gets.
// It is a function rather than an inline literal so what is registered — and,
// just as importantly, what is registered only when asked for — is assertable
// without standing up a whole engine (engine_test.go).
func localToolset(opts chatOpts, db libdbexec.DBManager, tracker libtracker.ActivityTracker, goIndex gointel.Index, gojaTools *gojatool.Toolset) map[string]taskengine.ToolsRepo {
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
		// Always registered: with no $CONTENOX_DIR/tools, goja carries only
		// goja_eval, a pure compute sandbox with no ambient I/O — its only
		// reach out is host.tool, gated by this same envelope. Script tools
		// get no rule, so operator-authored code falls to default_action.
		gojatool.ToolsProviderName: gojaTools,
		// Durable-only (no asker, no publisher): this engine resumes
		// suspended mission chains, whose later report/finish calls must
		// land in the store rather than fail as unknown tools.
		missiontools.ToolsProviderName: missiontools.New(missionservice.New(db), nil),
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

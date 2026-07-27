package contenoxcli

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/contenox/beam/internal/kernel/enginesvc"
	"github.com/contenox/beam/internal/kernel/taskengine"
	"github.com/contenox/beam/internal/libdbexec"
	"github.com/contenox/beam/internal/libtracker"
	"github.com/contenox/beam/internal/services/agentservice"
	"github.com/contenox/beam/internal/services/gointel"
	"github.com/contenox/beam/internal/services/hitlservice"
	"github.com/contenox/beam/internal/services/localtools"
	"github.com/contenox/beam/internal/services/missionservice"
	"github.com/contenox/beam/internal/services/missiontools"
	"github.com/contenox/beam/internal/services/setupcheck"
	"github.com/contenox/beam/internal/store/runtimetypes"
)

// ComputeReadiness builds the engine — which runs a read-only backend sync, NOT
// a model completion — and returns the evaluated setup readiness. It is the
// shared path behind `contenox doctor` and the setup wizard's final check, so
// readiness is verified without ever sending a prompt (which would run a chain,
// spend tokens, and could touch tools/audit trails). opts.EffectiveNoDeleteModels
// is honored by BuildEngine, so a readiness check never prunes models.
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
		tracker = libtracker.NewLogActivityTracker(slog.Default())
	}

	reportErr, reportChange, end := tracker.Start(ctx, "build", "engine")
	defer end()

	// The Go-intelligence index owns a reaper goroutine, so it needs a
	// lifecycle: it rides engine.Stop below, and the guard here covers every
	// error return between construction and that wrap.
	goIndex := gointel.NewIndex(gointel.Config{AllowedDir: opts.EffectiveLocalExecAllowedDir})
	engineBuilt := false
	defer func() {
		if !engineBuilt {
			goIndex.Shutdown()
		}
	}()

	tools := localToolset(opts, db, tracker, goIndex)

	askApproval := opts.EffectiveAskApproval
	if askApproval == nil {
		askApproval = NewCLIAskApproval(os.Stderr)
	}

	readinessModel, readinessProvider := readinessDefaults(opts)

	// Build the HITL service HERE and inject it, rather than letting enginesvc
	// mint one internally: the composition root must hold the instance so it can
	// register the resume-on-verdict hook after the engine exists (the engine is
	// a hook dependency, so registration cannot happen anywhere lower).
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
	})
	if err != nil {
		reportErr(err)
		return nil, err
	}
	if hitlSvc != nil {
		hitlservice.SetResumeHook(hitlSvc, agentservice.ResumeHook(agentservice.Deps{
			Engine:      engine,
			DB:          db,
			WorkspaceID: ResolveWorkspaceID(opts.ContenoxDir),
		}))
	}
	reportChange("phase", "enginesvc_built")
	// Ride the engine's chainable stop so the index's reaper is joined
	// whenever the engine goes down — the same pattern enginesvc uses for
	// the MCP manager.
	engineBuilt = true
	oldStop := engine.Stop
	engine.Stop = func() {
		goIndex.Shutdown()
		oldStop()
	}
	return engine, nil
}

// localToolset is the set of local tool providers every CLI-side engine gets.
// It is a function rather than an inline literal so what is registered — and,
// just as importantly, what is registered only when asked for — is assertable
// without standing up a whole engine (engine_test.go).
func localToolset(opts chatOpts, db libdbexec.DBManager, tracker libtracker.ActivityTracker, goIndex gointel.Index) map[string]taskengine.ToolsRepo {
	tools := map[string]taskengine.ToolsRepo{
		"echo":     localtools.NewEchoTools(),
		"print":    localtools.NewPrint(tracker),
		"webtools": localtools.NewWebCaller(tracker),
		"local_fs": localtools.NewLocalFSTools(opts.EffectiveLocalExecAllowedDir, db),
		// git is ALWAYS registered — unlike local_shell, it is not gated on
		// --shell. Reading the repository is the point: an agent that cannot see
		// what changed says "I can't do that" to the most ordinary question a
		// coding session has. What it may DO is the envelope's call, not this
		// file's: the seeded policies allow the six read operations and hold the
		// four that change the repository at an approval. Same directory scoping
		// as local_fs above, so both tools see one workspace.
		localtools.GitToolsName: localtools.NewGitTools(opts.EffectiveLocalExecAllowedDir),
		// Go intelligence is ALWAYS registered for the same reason git is:
		// asking "what type is this / who calls this" is reading, and every
		// gointel tool is a read (the seeded policies allow the whole
		// toolset — revisit that rule before any mutating op ever lands
		// here). Same directory scoping as local_fs and git.
		gointel.ToolsProviderName: gointel.NewTools(goIndex),
		// Durable-only mission tools (no asker, no publisher): this engine is
		// what `approvals respond` resumes suspended MISSION chains on, and a
		// resumed chain's later mission_report/mission_finish calls must land
		// in the store rather than fail as unknown tools. A nested attention
		// ask degrades to the durable blocker fallback here.
		missiontools.ToolsProviderName: missiontools.New(missionservice.New(db), nil),
	}
	if opts.EffectiveEnableLocalExec {
		execOpts := []localtools.LocalExecOption{}
		if opts.EffectiveLocalExecAllowedDir != "" {
			execOpts = append(execOpts, localtools.WithLocalExecAllowedDir(opts.EffectiveLocalExecAllowedDir))
		}
		tools["local_shell"] = localtools.NewLocalExecTools(execOpts...)

		if !opts.EffectiveHITL && opts.EffectiveLocalExecAllowedDir == "" {
			slog.Warn("local_shell is enabled with no HITL and no allowed-dir; chain-level tools_policies is the only safety gate")
		}
	}
	return tools
}

// readinessDefaults derives the effective default model/provider to credit during
// setup preflight. It credits the effective value whenever a flag override made it
// DIFFER from persisted config — not only when config is empty. The setup-readiness
// check must validate the model/provider the engine will ACTUALLY use for the turn
// (opts.EffectiveDefault*, which honor --model/--provider), so an explicit override
// to a healthy backend is not blocked by a broken persisted default: `--provider
// vertex-google` must run even when default-provider=ollama is configured but unservable.
// A model equal to the hardcoded fallback with no persisted config is still treated
// as unset (matching the flag-vs-config precedence in cli.go/run_cmd.go); when the
// effective value equals persisted config, the check sees that value directly and no
// override is needed.
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

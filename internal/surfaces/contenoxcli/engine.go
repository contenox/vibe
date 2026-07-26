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
	"github.com/contenox/beam/internal/services/hitlservice"
	"github.com/contenox/beam/internal/services/localtools"
	"github.com/contenox/beam/internal/services/setupcheck"
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

	tools := map[string]taskengine.ToolsRepo{
		"echo":     localtools.NewEchoTools(),
		"print":    localtools.NewPrint(tracker),
		"webtools": localtools.NewWebCaller(tracker),
		"local_fs": localtools.NewLocalFSTools(opts.EffectiveLocalExecAllowedDir, db),
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
	reportChange("phase", "enginesvc_built")
	return engine, nil
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

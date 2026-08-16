package contenoxcli

import (
	"context"
	"fmt"
	"os"

	"github.com/contenox/contenox/internal/kernel/enginesvc"
	"github.com/contenox/contenox/internal/kernel/taskengine"
	"github.com/contenox/contenox/internal/services/agentservice"
	"github.com/contenox/contenox/internal/services/eventlog"
	"github.com/contenox/contenox/internal/services/hitlservice"
	"github.com/contenox/contenox/internal/services/missionservice"
	"github.com/contenox/contenox/internal/services/missiontools"
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

	// One bus for this process: the mission tools below must publish on the same bus the engine runs on, or a resumed unit's report/mission_finish reaches nothing.
	bus := libbus.NewSQLite(db.WithoutTransaction())

	engineBuilt := false
	defer func() {
		if !engineBuilt {
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
	var missionOpts []missiontools.Option
	if hitlSvc != nil {
		missionOpts = append(missionOpts, missiontools.WithAttentionAsker(missionAttentionAsker{hitl: hitlSvc, missions: missions, bus: missionPub}))
	}
	tools := localToolset(opts, db, tracker, missions, missionOpts...)

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
	})
	if err != nil {
		reportErr(err)
		return nil, err
	}
	// Same ordering for local_fs's audio seam: read_file on an audio file transcribes through the engine's chat path from here on.
	trigHook.Set(buildInProcessTriggerHook(ctx, db, opts.ContenoxDir, workspaceID, engine, opts, os.Stderr))
	if hitlSvc != nil {
		hitlservice.SetResumeHook(hitlSvc, agentservice.ResumeHook(agentservice.Deps{
			Engine:      engine,
			DB:          db,
			WorkspaceID: workspaceID,
		}))
	}
	reportChange("phase", "enginesvc_built")
	engineBuilt = true
	oldStop := engine.Stop
	engine.Stop = func() {
		// Draining before teardown keeps at-least-once true: a firing claims its row before running, so an exit mid-firing would strand the claim otherwise.
		trigHook.Drain(eventlog.DefaultDrainTimeout)
		oldStop()
		// Ours to close: enginesvc closes only a bus it minted itself.
		bus.Close()
	}
	return engine, nil
}

func localToolset(opts chatOpts, db libdbexec.DBManager, tracker libtracker.ActivityTracker, missions missionservice.Service, missionOpts ...missiontools.Option) map[string]taskengine.ToolsRepo {
	tools := map[string]taskengine.ToolsRepo{
		// Wired, not durable-only: this engine resumes suspended mission chains, and a resumed unit asks its remaining questions here.
		missiontools.ToolsProviderName: missiontools.New(missions, missionOpts...),
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

// hitlPolicySource loads envelopes over policyDirs, so what a unit actually
// loads is what /mission offers and what `contenox vet` lints — including the
// envelopes rendered from agent declarations.
func hitlPolicySource(primaryDir string) hitlservice.PolicySource {
	return hitlservice.NewFSPolicySource(policyDirs(primaryDir)...)
}

func newHITLService(contenoxDir string, store runtimetypes.Store, tracker libtracker.ActivityTracker, fallbackPolicy string) hitlservice.Service {
	svc := hitlservice.NewWithDefaultPolicy(hitlPolicySource(contenoxDir), runtimetypes.LocalTenantID, store, tracker, fallbackPolicy)
	hitlservice.SetWorkspaceID(svc, ResolveWorkspaceID(contenoxDir))
	return svc
}

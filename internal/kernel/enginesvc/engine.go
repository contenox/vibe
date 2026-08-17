package enginesvc

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/contenox/contenox/internal/kernel/taskengine"
	"github.com/contenox/contenox/internal/kernel/tools"
	"github.com/contenox/contenox/internal/models/llmrepo"
	"github.com/contenox/contenox/internal/models/ollamatokenizer"
	"github.com/contenox/contenox/internal/models/runtimestate"
	"github.com/contenox/contenox/internal/services/clikv"
	"github.com/contenox/contenox/internal/services/execservice"
	"github.com/contenox/contenox/internal/services/hitlservice"
	"github.com/contenox/contenox/internal/services/localtools"
	"github.com/contenox/contenox/internal/services/mcpworker"
	"github.com/contenox/contenox/internal/services/missiontools"
	"github.com/contenox/contenox/internal/services/setupcheck"
	"github.com/contenox/contenox/internal/services/stateservice"
	"github.com/contenox/contenox/internal/services/toolguidance"
	"github.com/contenox/contenox/internal/store/runtimetypes"
	"github.com/contenox/contenox/internal/substrate"
	libbus "github.com/contenox/contenox/libbus"
	"github.com/contenox/contenox/libdbexec"
	"github.com/contenox/contenox/libtracker"
)

// LocalTenantID re-exports runtimetypes.LocalTenantID for backwards compatibility.
const LocalTenantID = runtimetypes.LocalTenantID

func Build(ctx context.Context, db libdbexec.DBManager, cfg Config) (*Engine, error) {
	engineCtx, engineCancel := context.WithCancel(ctx)

	bus := cfg.Bus
	ownsBus := false
	if bus == nil {
		opened, err := substrate.OpenBus(engineCtx, db.WithoutTransaction())
		if err != nil {
			engineCancel()
			return nil, err
		}
		bus, ownsBus = opened, true
	}

	closeBus := func() {
		if ownsBus {
			bus.Close()
		}
	}

	releaseKV := func() {}

	success := false
	defer func() {
		if !success {
			engineCancel()
			closeBus()
			releaseKV()
		}
	}()

	kvMgr := cfg.KVStore
	if kvMgr == nil {
		opened, release, err := substrate.OpenKV(engineCtx, db)
		if err != nil {
			return nil, err
		}
		kvMgr, releaseKV = opened, release
	}

	state := cfg.State
	if state == nil {
		stateOpts := []runtimestate.Option{
			runtimestate.WithKVStore(kvMgr),
			runtimestate.WithAutoDiscoverModels(),
		}
		if cfg.NoDeleteModels {
			stateOpts = append(stateOpts, runtimestate.WithSkipDeleteUndeclaredModels())
		}
		var err error
		state, err = runtimestate.New(engineCtx, db, bus, stateOpts...)
		if err != nil {
			return nil, fmt.Errorf("failed to create runtime state: %w", err)
		}
	}
	engine := &Engine{Stop: func() {
		engineCancel()
		closeBus()
		releaseKV()
	}, Bus: bus, State: state}

	tenantID := cfg.TenantID
	if tenantID == "" {
		tenantID = runtimetypes.LocalTenantID
	}
	config := &runtimestate.Config{
		TenantID:   tenantID,
		EmbedModel: cfg.DefaultModel,
		TaskModel:  cfg.DefaultModel,
		ChatModel:  cfg.DefaultModel,
	}
	if err := runtimestate.InitEmbeder(ctx, config, db, cfg.ContextLength, state); err != nil {
		return nil, fmt.Errorf("failed to init embedder: %w", err)
	}
	if err := runtimestate.InitPromptExec(ctx, config, db, state, cfg.ContextLength); err != nil {
		return nil, fmt.Errorf("failed to init prompt executor: %w", err)
	}
	if err := runtimestate.InitChatExec(ctx, config, db, state, cfg.ContextLength); err != nil {
		return nil, fmt.Errorf("failed to init chat executor: %w", err)
	}

	specs := []runtimestate.ExtraModelSpec{
		{
			Name:          cfg.DefaultModel,
			ContextLength: cfg.ContextLength,
			CanChat:       true,
			CanPrompt:     true,
			CanEmbed:      false,
		},
	}
	if err := runtimestate.EnsureModels(ctx, db, tenantID, specs); err != nil {
		return nil, fmt.Errorf("failed to ensure models: %w", err)
	}

	tracker := cfg.Tracker
	if tracker == nil {
		if cfg.Tracing {
			// nil, not slog.Default(): the constructor defaults it to the
			// same logger, keeping the kernel out of the slog import graph.
			tracker = libtracker.NewLogActivityTracker(nil)
		} else {
			tracker = libtracker.NoopTracker{}
		}
	}

	if !cfg.SkipBackendCycle {
		cycleReportErr, _, cycleEnd := tracker.Start(ctx, "sync", "backend_cycle")
		if err := state.RunBackendCycle(ctx); err != nil {
			cycleReportErr(err)
		}
		cycleEnd()
	}
	rt := state.Get(ctx)
	anyReachable := false
	_, reportReachable, reachableEnd := tracker.Start(ctx, "check", "backend_reachability")
	for id, bs := range rt {
		if bs.Error != "" {
			reportReachable(id, map[string]any{"url": bs.Backend.BaseURL, "error": bs.Error})
		} else {
			anyReachable = true
		}
	}
	if !anyReachable {
		reportReachable("", "no reachable backends; subsequent model operations may fail")
	}
	reachableEnd()

	ss := stateservice.New(state, db, cfg.WorkspaceID)
	setupStatus := func(ctx context.Context) (setupcheck.Result, error) {
		r, err := ss.SetupStatus(ctx)
		if err != nil {
			return setupcheck.Result{}, err
		}
		return setupcheck.OverlayEffectiveDefaults(r, cfg.ReadinessDefaultModel, cfg.ReadinessDefaultProvider), nil
	}
	res, err := setupStatus(ctx)
	if err != nil {
		return nil, fmt.Errorf("setup status failed: %w", err)
	}
	engine.SetupCheck = res
	engine.SetupStatus = setupStatus

	tokenizer := ollamatokenizer.NewEstimateTokenizer()

	audio := resolveAudioModel(ctx, runtimetypes.New(db.WithoutTransaction()), cfg)

	repo, err := llmrepo.NewModelManager(state, tokenizer, llmrepo.ModelManagerConfig{
		DefaultPromptModel: llmrepo.ModelConfig{Name: cfg.DefaultModel, Provider: cfg.DefaultProvider},
		DefaultChatModel:   llmrepo.ModelConfig{Name: cfg.DefaultModel, Provider: cfg.DefaultProvider},
		DefaultAudioModel:  audio,
	}, tracker)
	if err != nil {
		return nil, fmt.Errorf("failed to create model manager: %w", err)
	}
	engine.Models = repo
	engine.AudioModel = audio

	eventSink := cfg.TaskEventSink
	if eventSink == nil {
		eventSink = taskengine.NewBusTaskEventSink(bus)
	}
	cfg.TaskEventSink = eventSink

	mgr, localToolNames, toolsRepo, err := buildTools(engineCtx, cfg, db, tracker, bus)
	if err != nil {
		return nil, err
	}

	execCtx := taskengine.WithTaskEventSink(engineCtx, eventSink)

	exec, err := taskengine.NewExec(execCtx, repo, toolsRepo, tracker)
	if err != nil {
		return nil, fmt.Errorf("failed to create task executor: %w", err)
	}
	var inspector taskengine.Inspector = taskengine.NewSimpleInspector()
	inspector = taskengine.NewKVInspector(inspector, kvMgr, tracker)
	inspector = taskengine.NewBusInspector(inspector, bus, tracker)
	for _, wrap := range cfg.ExtraInspectors {
		inspector = wrap(inspector)
	}
	envExec, err := taskengine.NewEnv(execCtx, tracker, exec, inspector, toolsRepo)
	if err != nil {
		return nil, fmt.Errorf("failed to create environment executor: %w", err)
	}
	envExec, err = taskengine.NewMacroEnv(envExec, toolsRepo)
	if err != nil {
		return nil, fmt.Errorf("failed to create macro environment: %w", err)
	}
	taskService := execservice.NewTasksEnv(engineCtx, envExec, toolsRepo)

	engine.TaskService = taskService
	engine.Tracker = tracker
	engine.TaskEventSink = eventSink
	engine.MCPManager = mgr
	engine.LocalTools = localToolNames

	oldStop := engine.Stop
	engine.Stop = func() {
		mgr.StopAll()
		oldStop()
	}
	success = true
	return engine, nil
}

func resolveAudioModel(ctx context.Context, store runtimetypes.Store, cfg Config) llmrepo.ModelConfig {
	model := strings.TrimSpace(cfg.DefaultAudioModel)
	if model == "" {
		model = clikv.Read(ctx, store, "default-audio-model")
	}
	provider := strings.TrimSpace(cfg.DefaultAudioProvider)
	if provider == "" {
		provider = clikv.Read(ctx, store, "default-audio-provider")
	}
	if model == "" {
		// A provider without a model is not a usable role; resolution treats
		// the whole role as unset rather than pinning a provider by accident.
		return llmrepo.ModelConfig{}
	}
	return llmrepo.ModelConfig{Name: model, Provider: provider}
}

func buildTools(engineCtx context.Context, cfg Config, db libdbexec.DBManager, tracker libtracker.ActivityTracker, bus libbus.Messenger) (*mcpworker.Manager, []string, taskengine.ToolsRepo, error) {
	store := runtimetypes.New(db.WithoutTransaction())
	mgr, err := mcpworker.New(engineCtx, store, bus, tracker)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("failed to create mcp worker manager: %w", err)
	}
	if err := mgr.WatchEvents(engineCtx); err != nil {
		return nil, nil, nil, fmt.Errorf("failed to start mcp event watcher: %w", err)
	}

	localToolNames := make([]string, 0, len(cfg.LocalTools))
	for name := range cfg.LocalTools {
		localToolNames = append(localToolNames, name)
	}
	toolsRepo := tools.NewPersistentRepo(cfg.LocalTools, db, http.DefaultClient, bus, tracker)

	if cfg.EnableHITL {
		if cfg.AskApproval == nil {
			return nil, nil, nil, fmt.Errorf("enginesvc: EnableHITL is true but AskApproval callback is nil")
		}
		hitlSvc := cfg.HITLService
		if hitlSvc == nil {
			hitlTenant := cfg.TenantID
			if hitlTenant == "" {
				hitlTenant = runtimetypes.LocalTenantID
			}
			hitlSvc = hitlservice.NewWithDefaultPolicy(cfg.HITLPolicySource, hitlTenant, store, tracker, cfg.HITLDefaultPolicyName)
		}
		// Runs unconditionally, injected hitlSvc included: the evaluator must
		// read this engine's workspace-scoped policy row, not the global one.
		hitlservice.SetWorkspaceID(hitlSvc, cfg.WorkspaceID)
		// Mission tools are exempted from the HITL gate by construction: they
		// are the attention channel itself, and gating them would deadlock an
		// unattended unit asking for its own report.
		raw := toolsRepo
		toolsRepo = hitlExemptProviders(
			localtools.NewHITLWrapper(toolsRepo, cfg.AskApproval, hitlSvc, tracker, cfg.TaskEventSink),
			raw,
			missiontools.ToolsProviderName,
		)
	}

	// Position matters: after the HITL wrap, before the guidance wrap, so a
	// nested tool caller meets the same gate the model meets.
	if cfg.OnToolsRepoReady != nil {
		cfg.OnToolsRepoReady(toolsRepo)
	}

	// Sits outside the HITL wrapper so it counts only model-level calls, not
	// the gate's internal diff reads.
	toolsRepo = toolguidance.WrapFromEnv(toolsRepo)

	return mgr, localToolNames, toolsRepo, nil
}

type hitlExemptRepo struct {
	taskengine.ToolsRepo
	raw    taskengine.ToolsRepo
	exempt map[string]bool
}

func hitlExemptProviders(gated, raw taskengine.ToolsRepo, providers ...string) taskengine.ToolsRepo {
	exempt := make(map[string]bool, len(providers))
	for _, p := range providers {
		exempt[p] = true
	}
	return &hitlExemptRepo{ToolsRepo: gated, raw: raw, exempt: exempt}
}

func (h *hitlExemptRepo) Exec(ctx context.Context, startingTime time.Time, input any, debug bool, args *taskengine.ToolsCall) (any, taskengine.DataType, error) {
	if args != nil && h.exempt[args.Name] {
		return h.raw.Exec(ctx, startingTime, input, debug, args)
	}
	return h.ToolsRepo.Exec(ctx, startingTime, input, debug, args)
}

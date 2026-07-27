package enginesvc

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/contenox/beam/internal/kernel/taskengine"
	"github.com/contenox/beam/internal/kernel/tools"
	libbus "github.com/contenox/beam/internal/libbus"
	"github.com/contenox/beam/internal/libdbexec"
	"github.com/contenox/beam/internal/libkvstore"
	"github.com/contenox/beam/internal/libtracker"
	"github.com/contenox/beam/internal/models/llmrepo"
	"github.com/contenox/beam/internal/models/ollamatokenizer"
	"github.com/contenox/beam/internal/models/runtimestate"
	"github.com/contenox/beam/internal/services/clikv"
	"github.com/contenox/beam/internal/services/execservice"
	"github.com/contenox/beam/internal/services/hitlservice"
	"github.com/contenox/beam/internal/services/localtools"
	"github.com/contenox/beam/internal/services/mcpworker"
	"github.com/contenox/beam/internal/services/missiontools"
	"github.com/contenox/beam/internal/services/setupcheck"
	"github.com/contenox/beam/internal/services/stateservice"
	"github.com/contenox/beam/internal/services/toolguidance"
	"github.com/contenox/beam/internal/store/runtimetypes"
)

// LocalTenantID is re-exported from runtimetypes for backwards compatibility.
// New code should reference runtimetypes.LocalTenantID directly.
const LocalTenantID = runtimetypes.LocalTenantID

func Build(ctx context.Context, db libdbexec.DBManager, cfg Config) (*Engine, error) {
	engineCtx, engineCancel := context.WithCancel(ctx)

	bus := cfg.Bus
	ownsBus := false
	if bus == nil {
		bus = libbus.NewSQLite(db.WithoutTransaction())
		ownsBus = true
	}

	closeBus := func() {
		if ownsBus {
			bus.Close()
		}
	}

	success := false
	defer func() {
		if !success {
			engineCancel()
			closeBus()
		}
	}()

	kvMgr := cfg.KVStore
	if kvMgr == nil {
		kvMgr = libkvstore.NewSQLiteManager(db)
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
			// nil, not slog.Default(): the constructor defaults it to the same
			// logger, and passing nil keeps the kernel out of the slog import
			// graph entirely — one fewer place the guard has to make an
			// exception for, and one fewer place a slog.Warn could be added
			// beside a legitimate use and look at home.
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
	// setupStatus wraps the pure DB-config readiness check so that effective
	// defaults supplied out-of-band (CLI --model/--provider) satisfy preflight
	// even when they were never persisted to KV config. Both the build-time
	// snapshot and the live recompute go through the same overlay.
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

	embedding := resolveEmbeddingModel(ctx, runtimetypes.New(db.WithoutTransaction()), cfg, tracker)

	repo, err := llmrepo.NewModelManager(state, tokenizer, llmrepo.ModelManagerConfig{
		DefaultPromptModel:    llmrepo.ModelConfig{Name: cfg.DefaultModel, Provider: cfg.DefaultProvider},
		DefaultEmbeddingModel: embedding,
		DefaultChatModel:      llmrepo.ModelConfig{Name: cfg.DefaultModel, Provider: cfg.DefaultProvider},
	}, tracker)
	if err != nil {
		return nil, fmt.Errorf("failed to create model manager: %w", err)
	}
	engine.Models = repo
	engine.EmbeddingModel = embedding

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

// resolveEmbeddingModel decides which model produces EMBEDDINGS.
//
// This used to be the chat model, unconditionally (DefaultEmbeddingModel was set
// to cfg.DefaultModel) — correct only where a provider's chat model also embeds,
// and silently wrong everywhere else: Ollama's chat models mostly refuse /embed,
// and OpenAI's chat models are not embedding models at all. Retrieval built on
// that assumption fails at the provider with an error that names the wrong
// thing.
//
// Resolution order, per docs/development/blueprints/workspace-index.md:
//
//  1. the caller's explicit Config fields (a flag, an embedder wiring its own),
//  2. the `contenox config` KV keys default-embed-model / default-embed-provider,
//  3. the chat model, WITH a warning — retrieval is optional, so an unset embed
//     model must never fail Build; it degrades, loudly.
//
// "Loudly" means through the tracker, which is the only instrumentation seam
// here: it is the seam that redacts, and the one an embedding host can point at
// something other than a log file. The fallback is reported as a CHANGE rather
// than an error because nothing failed — the resolution succeeded, on a degraded
// input — which is the same judgement the reachability report in Build makes.
//
// The provider falling back alone is silent by design: one provider serving both
// the chat and the embedding model is the normal single-backend setup, and
// warning about it would train operators to ignore the warning that matters.
func resolveEmbeddingModel(ctx context.Context, store runtimetypes.Store, cfg Config, tracker libtracker.ActivityTracker) llmrepo.ModelConfig {
	if tracker == nil {
		tracker = libtracker.NoopTracker{}
	}
	model := strings.TrimSpace(cfg.DefaultEmbedModel)
	if model == "" {
		model = clikv.Read(ctx, store, "default-embed-model")
	}
	provider := strings.TrimSpace(cfg.DefaultEmbedProvider)
	if provider == "" {
		provider = clikv.Read(ctx, store, "default-embed-provider")
	}

	if model == "" {
		_, reportFallback, endFallback := tracker.Start(ctx, "resolve", "embedding_model",
			"fallback_model", cfg.DefaultModel,
			"fallback_provider", cfg.DefaultProvider,
			"hint", "contenox config set default-embed-model <name>  # a chat model embeds only on some providers")
		reportFallback(cfg.DefaultModel, "no embedding model configured; falling back to the chat model")
		endFallback()
		model = cfg.DefaultModel
	}
	if provider == "" {
		provider = cfg.DefaultProvider
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
		// The mission tools are exempted from the HITL gate BY CONSTRUCTION, not
		// by policy data. They are the attention channel itself: mission_report /
		// mission_ask_attention / mission_finish / mission_plan are how an
		// unattended unit reaches its operator — gating them behind the approval
		// machinery they exist to carry is a deadlock, and a live one: a
		// discovered chain unit (HITL on) under a default_action:"approve" policy
		// raised an ask FOR ITS OWN REPORT and hung until --wait timed out. The
		// tools are already scoped harder than any policy could scope them (a
		// session without a mission id cannot even see them), and their writes go
		// only to the mission store. Exempting here — rather than allow-listing
		// them in every shipped policy — means no operator-authored policy can
		// ever reintroduce the deadlock by omission.
		raw := toolsRepo
		toolsRepo = hitlExemptProviders(
			localtools.NewHITLWrapper(toolsRepo, cfg.AskApproval, hitlSvc, tracker, cfg.TaskEventSink),
			raw,
			missiontools.ToolsProviderName,
		)
	}

	// Late-bind whoever needed the aggregate repo they are themselves registered
	// in (Config.OnToolsRepoReady documents the cycle and why this exact repo).
	// The call sits HERE — after the HITL wrap, before the guidance wrap — and
	// that position is the contract, not an implementation detail: a nested tool
	// caller must meet the same gate the model meets, and must not be counted as
	// model-level navigation.
	if cfg.OnToolsRepoReady != nil {
		cfg.OnToolsRepoReady(toolsRepo)
	}

	// Attention layer, Stage 0 — the inward face. This is THE single seam: one
	// decorator over the aggregate tools repo observes every provider (local_fs,
	// local_shell, webtools, mission, MCP) without touching them, and feeds
	// navigation-awareness back to the model through the tool-result envelope. It
	// sits OUTSIDE the HITL wrapper so it counts only model-level calls, not the
	// internal reads the gate makes to build a diff. On by default (the blind-spot
	// doctrine); CONTENOX_TOOL_GUIDANCE=off returns the inner repo untouched.
	// Because acp/serve/chat all compose their tools through this one buildTools,
	// this single wrap covers every entry path.
	toolsRepo = toolguidance.WrapFromEnv(toolsRepo)

	return mgr, localToolNames, toolsRepo, nil
}

// hitlExemptRepo routes Exec calls for exempted PROVIDERS around the HITL
// wrapper to the raw aggregate, and everything else — including listings and
// schemas — through the gated repo unchanged. See the construction site in
// buildTools for why exemption is structural rather than policy data.
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

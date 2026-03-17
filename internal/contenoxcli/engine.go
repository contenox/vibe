package contenoxcli

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/contenox/contenox/eventsourceservice"
	"github.com/contenox/contenox/execservice"
	"github.com/contenox/contenox/executor"
	"github.com/contenox/contenox/functionservice"
	"github.com/contenox/contenox/internal/eventdispatch"
	"github.com/contenox/contenox/internal/hooks"
	"github.com/contenox/contenox/internal/llmrepo"
	"github.com/contenox/contenox/internal/ollamatokenizer"
	"github.com/contenox/contenox/internal/runtimestate"
	"github.com/contenox/contenox/jseval"
	libbus "github.com/contenox/contenox/libbus"
	"github.com/contenox/contenox/libdbexec"
	"github.com/contenox/contenox/libkvstore"
	"github.com/contenox/contenox/libtracker"
	"github.com/contenox/contenox/localhooks"
	"github.com/contenox/contenox/mcpworker"
	"github.com/contenox/contenox/runtimetypes"
	"github.com/contenox/contenox/taskchainservice"
	"github.com/contenox/contenox/taskengine"
)

type Engine struct {
	TaskService execservice.TasksEnvService
	Tracker     libtracker.ActivityTracker
	JSEnv       *jseval.Env
	Stop        func()
	MCPManager  *mcpworker.Manager
	// LocalHooks lists the names of all registered local hook handlers.
	LocalHooks []string
}

// BuildEngine scaffolds the complex dependency graph needed to run task chains.
func BuildEngine(ctx context.Context, db libdbexec.DBManager, opts chatOpts) (*Engine, error) {
	// Derive a cancellable context owned by this engine instance.
	// Cancelling it unblocks all goroutines (WatchEvents, bus streams, etc.)
	// before bus.Close() is called, preventing the process from hanging.
	engineCtx, engineCancel := context.WithCancel(ctx)

	// SQLite-backed bus (same architecture as runtime-API, just without NATS)
	bus := libbus.NewSQLite(db.WithoutTransaction())

	// Armed defer: if we return early on error, cancel the engine context and
	// close the bus so no goroutines are leaked.
	success := false
	defer func() {
		if !success {
			engineCancel()
			bus.Close()
		}
	}()

	engine := &Engine{Stop: func() {
		engineCancel() // signal all goroutines to stop
		bus.Close()
	}}

	// Runtime state — always enable auto-discover for the CLI so users don't
	// need to run 'model add' before using Ollama, OpenAI or vLLM models.
	// The fleet-management runtime-api (Dockerfile) does NOT use this option.
	stateOpts := []runtimestate.Option{
		runtimestate.WithAutoDiscoverModels(),
	}
	if opts.EffectiveNoDeleteModels {
		stateOpts = append(stateOpts, runtimestate.WithSkipDeleteUndeclaredModels())
	}
	// Wire the SQLite-backed KV store so the provider model-list cache (Gemini/OpenAI)
	// survives across CLI invocations.
	kvMgr := libkvstore.NewSQLiteManager(db)
	stateOpts = append(stateOpts, runtimestate.WithKVStore(kvMgr))
	state, err := runtimestate.New(engineCtx, db, bus, stateOpts...)
	if err != nil {
		return nil, fmt.Errorf("failed to create runtime state: %w", err)
	}

	// 4. Initialize embed/task/chat groups
	config := &runtimestate.Config{
		TenantID:   localTenantID,
		EmbedModel: opts.EffectiveDefaultModel,
		TaskModel:  opts.EffectiveDefaultModel,
		ChatModel:  opts.EffectiveDefaultModel,
	}
	if err := runtimestate.InitEmbeder(ctx, config, db, opts.EffectiveContext, state); err != nil {
		return nil, fmt.Errorf("failed to init embedder: %w", err)
	}
	if err := runtimestate.InitPromptExec(ctx, config, db, state, opts.EffectiveContext); err != nil {
		return nil, fmt.Errorf("failed to init prompt executor: %w", err)
	}
	if err := runtimestate.InitChatExec(ctx, config, db, state, opts.EffectiveContext); err != nil {
		return nil, fmt.Errorf("failed to init chat executor: %w", err)
	}

	// 4b. Ensure the default model is registered in the local tenant.
	specs := []runtimestate.ExtraModelSpec{
		{
			Name:          opts.EffectiveDefaultModel,
			ContextLength: opts.EffectiveContext, // 0 = unknown, resolver won't filter on context
			CanChat:       true,
			CanPrompt:     true,
			CanEmbed:      false,
		},
	}
	if len(specs) > 0 {
		if err := runtimestate.EnsureModels(ctx, db, localTenantID, specs); err != nil {
			return nil, fmt.Errorf("failed to ensure models: %w", err)
		}
	}

	// 5. Backends are already in SQLite from `contenox backend add`; just run the sync cycle.
	// 6. Run backend cycle
	if opts.EffectiveTracing {
		slog.Info("Running backend cycle to sync models...")
	}
	if err := state.RunBackendCycle(ctx); err != nil {
		slog.Warn("Backend cycle encountered errors", "error", err)
	}
	rt := state.Get(ctx)
	anyReachable := false
	for id, bs := range rt {
		if bs.Error != "" {
			if opts.EffectiveTracing {
				slog.Warn("Backend unreachable", "id", id, "url", bs.Backend.BaseURL, "error", bs.Error)
			}
		} else {
			anyReachable = true
		}
	}
	if !anyReachable && opts.EffectiveTracing {
		slog.Warn("No reachable backends – subsequent model operations may fail")
	}

	// 7. Tokenizer and model manager
	tokenizer := ollamatokenizer.NewEstimateTokenizer()
	var tracker libtracker.ActivityTracker
	if opts.EffectiveTracing {
		tracker = libtracker.NewLogActivityTracker(slog.Default())
	} else {
		tracker = libtracker.NoopTracker{}
	}
	repo, err := llmrepo.NewModelManager(state, tokenizer, llmrepo.ModelManagerConfig{
		DefaultPromptModel:    llmrepo.ModelConfig{Name: opts.EffectiveDefaultModel, Provider: opts.EffectiveDefaultProvider},
		DefaultEmbeddingModel: llmrepo.ModelConfig{Name: opts.EffectiveDefaultModel, Provider: opts.EffectiveDefaultProvider},
		DefaultChatModel:      llmrepo.ModelConfig{Name: opts.EffectiveDefaultModel, Provider: opts.EffectiveDefaultProvider},
	}, tracker)
	if err != nil {
		return nil, fmt.Errorf("failed to create model manager: %w", err)
	}

	// 8. Local hooks
	jsEnv := jseval.NewEnv(tracker, jseval.BuiltinHandlers{}, jseval.DefaultBuiltins())
	localHooks := map[string]taskengine.HookRepo{
		"echo":         localhooks.NewEchoHook(),
		"print":        localhooks.NewPrint(tracker),
		"webhook":      localhooks.NewWebCaller(),
		"js_execution": localhooks.NewJSSandboxHook(jsEnv, tracker),
		"local_fs":     localhooks.NewLocalFSHook(opts.EffectiveLocalExecAllowedDir),
	}
	jsHooks := map[string]taskengine.HookRepo{
		"echo":         localhooks.NewEchoHook(),
		"print":        localhooks.NewPrint(tracker),
		"webhook":      localhooks.NewWebCaller(),
		"js_execution": localhooks.NewJSSandboxHook(jsEnv, tracker),
	}
	if sshHook, err := localhooks.NewSSHHook(); err != nil {
		slog.Debug("SSH hook not registered", "error", err)
	} else {
		jsHooks["ssh"] = sshHook
	}
	if opts.EffectiveEnableLocalExec {
		hookOpts := []localhooks.LocalExecOption{}
		if opts.EffectiveLocalExecAllowedDir != "" {
			hookOpts = append(hookOpts, localhooks.WithLocalExecAllowedDir(opts.EffectiveLocalExecAllowedDir))
		}
		localExecHook := localhooks.NewLocalExecHook(hookOpts...)
		jsHooks["local_shell"] = localExecHook
		localHooks["local_shell"] = localExecHook
	}
	// Wrap mutating local hooks with the HITL approval gate when the vibe TUI
	// wants interactive confirmation before every file write or shell command.
	if opts.AskApproval != nil {
		wrap := func(inner taskengine.HookRepo, tools map[string]bool) taskengine.HookRepo {
			return &localhooks.HITLWrapper{
				Inner:          inner,
				Ask:            opts.AskApproval,
				RequireApprove: tools,
			}
		}
		if h, ok := localHooks["local_fs"]; ok {
			localHooks["local_fs"] = wrap(h, map[string]bool{
				"write_file": true,
				"sed":        true,
			})
		}
		if h, ok := localHooks["local_shell"]; ok {
			localHooks["local_shell"] = wrap(h, map[string]bool{
				"local_shell": true,
			})
			jsHooks["local_shell"] = localHooks["local_shell"]
		}
	}
	// Start mcpworker.Manager — loads MCP servers from SQLite and serves them
	// via the SQLite bus. This is the same code path as the runtime-API (which uses NATS).
	store := runtimetypes.New(db.WithoutTransaction())
	mgr, err := mcpworker.New(engineCtx, store, bus, tracker)
	if err != nil {
		bus.Close()
		return nil, fmt.Errorf("failed to create mcp worker manager: %w", err)
	}
	if err := mgr.WatchEvents(engineCtx); err != nil {
		bus.Close()
		return nil, fmt.Errorf("failed to start mcp event watcher: %w", err)
	}
	engine.MCPManager = mgr
	for name := range localHooks {
		engine.LocalHooks = append(engine.LocalHooks, name)
	}
	hookRepo := hooks.NewPersistentRepo(localHooks, db, http.DefaultClient, bus)
	jsHookRepo := hooks.NewSimpleProvider(jsHooks)

	// 9. Task engine
	exec, err := taskengine.NewExec(engineCtx, repo, hookRepo, tracker)
	if err != nil {
		return nil, fmt.Errorf("failed to create task executor: %w", err)
	}
	envExec, err := taskengine.NewEnv(engineCtx, tracker, exec, taskengine.NewSimpleInspector(), hookRepo)
	if err != nil {
		return nil, fmt.Errorf("failed to create environment executor: %w", err)
	}
	envExec, err = taskengine.NewMacroEnv(envExec, hookRepo)
	if err != nil {
		return nil, fmt.Errorf("failed to create macro environment: %w", err)
	}
	taskService := execservice.NewTasksEnv(engineCtx, envExec, hookRepo)
	// Register plan_manager after taskService is built.
	// PersistentRepo holds localHooks by map reference, so this addition is
	// visible to hookRepo and all callers going forward.
	if opts.PlannerChain != nil && opts.ExecutorChain != nil {
		planHook := localhooks.NewPlanManagerHook(
			db, opts.PlannerChain, opts.ExecutorChain, taskService, opts.ContenoxDir,
		)
		if opts.AskApproval != nil {
			planHook = &localhooks.HITLWrapper{
				Inner: planHook,
				Ask:   opts.AskApproval,
				RequireApprove: map[string]bool{
					"run_next_step": true,
				},
			}
		}
		localHooks["plan_manager"] = planHook
		engine.LocalHooks = append(engine.LocalHooks, "plan_manager")
	}
	execSvc := execservice.NewExec(engineCtx, repo)
	chainSvc := taskchainservice.New(db)
	functionSvc := functionservice.New(db)
	gojaExec := executor.NewGojaExecutor(tracker, functionSvc)
	dispatcher, err := eventdispatch.New(engineCtx, functionSvc, func(ctx context.Context, err error) {
		slog.ErrorContext(ctx, "event dispatch error", "error", err)
	}, time.Second, gojaExec, tracker)
	if err != nil {
		return nil, fmt.Errorf("failed to create event dispatcher: %w", err)
	}
	_ = dispatcher
	eventsource := eventsourceservice.NewNoopService()
	gojaExec.AddBuildInServices(eventsource, execSvc, chainSvc, taskService, jsHookRepo)
	gojaExec.StartSync(engineCtx, time.Second*3)

	jsEnv.SetBuiltinHandlers(jseval.BuiltinHandlers{
		Eventsource:          eventsource,
		TaskService:          execSvc,
		TaskchainService:     chainSvc,
		TaskchainExecService: taskService,
		FunctionService:      functionSvc,
		HookRepo:             jsHookRepo,
	})

	engine.TaskService = taskService
	engine.Tracker = tracker
	engine.JSEnv = jsEnv

	oldStop := engine.Stop
	engine.Stop = func() {
		gojaExec.StopSync()
		mgr.StopAll() // terminates all stdio MCP child processes
		oldStop()
	}
	success = true
	return engine, nil
}

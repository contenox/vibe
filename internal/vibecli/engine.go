package vibecli

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/contenox/vibe/backendservice"
	"github.com/contenox/vibe/eventsourceservice"
	"github.com/contenox/vibe/execservice"
	"github.com/contenox/vibe/executor"
	"github.com/contenox/vibe/functionservice"
	"github.com/contenox/vibe/internal/eventdispatch"
	"github.com/contenox/vibe/internal/hooks"
	"github.com/contenox/vibe/internal/llmrepo"
	"github.com/contenox/vibe/internal/ollamatokenizer"
	"github.com/contenox/vibe/internal/runtimestate"
	"github.com/contenox/vibe/jseval"
	libbus "github.com/contenox/vibe/libbus"
	"github.com/contenox/vibe/libdbexec"
	"github.com/contenox/vibe/libtracker"
	"github.com/contenox/vibe/localhooks"
	"github.com/contenox/vibe/taskchainservice"
	"github.com/contenox/vibe/taskengine"
)

type Engine struct {
	TaskService execservice.TasksEnvService
	JSEnv       *jseval.Env
	Stop        func()
}

// BuildEngine scaffolds the complex dependency graph needed to run task chains.
func BuildEngine(ctx context.Context, db libdbexec.DBManager, opts runOpts) (*Engine, error) {
	// 4. Ensure models
	// 2. In-memory bus
	bus := libbus.NewInMem()
	engine := &Engine{Stop: func() { bus.Close() }}

	// 3. Runtime state
	stateOpts := []runtimestate.Option{}
	if opts.EffectiveNoDeleteModels {
		stateOpts = append(stateOpts, runtimestate.WithSkipDeleteUndeclaredModels())
	}
	state, err := runtimestate.New(ctx, db, bus, stateOpts...)
	if err != nil {
		bus.Close()
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
		bus.Close()
		return nil, fmt.Errorf("failed to init embedder: %w", err)
	}
	if err := runtimestate.InitPromptExec(ctx, config, db, state, opts.EffectiveContext); err != nil {
		bus.Close()
		return nil, fmt.Errorf("failed to init prompt executor: %w", err)
	}
	if err := runtimestate.InitChatExec(ctx, config, db, state, opts.EffectiveContext); err != nil {
		bus.Close()
		return nil, fmt.Errorf("failed to init chat executor: %w", err)
	}

	// 4b. Ensure extra models from config
	specs := make([]runtimestate.ExtraModelSpec, 0, len(opts.Cfg.ExtraModels)+1)

	defaultContextLen := opts.EffectiveContext
	if defaultContextLen <= 0 {
		defaultContextLen = defaultContext
	}

	// Always ensure the default model is present
	specs = append(specs, runtimestate.ExtraModelSpec{
		Name:          opts.EffectiveDefaultModel,
		ContextLength: defaultContextLen,
		CanChat:       true,
		CanPrompt:     true,
		CanEmbed:      false,
	})

	if len(opts.Cfg.ExtraModels) > 0 {
		for _, e := range opts.Cfg.ExtraModels {
			if e.Context <= 0 {
				continue
			}
			canChat := true
			if e.CanChat != nil {
				canChat = *e.CanChat
			}
			canPrompt := true
			if e.CanPrompt != nil {
				canPrompt = *e.CanPrompt
			}
			canEmbed := false
			if e.CanEmbed != nil {
				canEmbed = *e.CanEmbed
			}
			specs = append(specs, runtimestate.ExtraModelSpec{
				Name:          e.Name,
				ContextLength: e.Context,
				CanChat:       canChat,
				CanPrompt:     canPrompt,
				CanEmbed:      canEmbed,
			})
		}
	}

	if len(specs) > 0 {
		if err := runtimestate.EnsureModels(ctx, db, localTenantID, specs); err != nil {
			bus.Close()
			return nil, fmt.Errorf("failed to ensure models: %w", err)
		}
	}

	// 5. Ensure backends
	backendSvc := backendservice.New(db)
	if err := ensureBackendsFromConfig(ctx, db, backendSvc, opts.ResolvedBackends); err != nil {
		bus.Close()
		return nil, fmt.Errorf("failed to ensure backends: %w", err)
	}

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
		if opts.EffectiveLocalExecAllowedCommands != "" {
			commands := splitAndTrim(opts.EffectiveLocalExecAllowedCommands, ",")
			if len(commands) > 0 {
				hookOpts = append(hookOpts, localhooks.WithLocalExecAllowedCommands(commands))
			}
		}
		if len(opts.EffectiveLocalExecDeniedCommands) > 0 {
			hookOpts = append(hookOpts, localhooks.WithLocalExecDeniedCommands(opts.EffectiveLocalExecDeniedCommands))
		}
		localExecHook := localhooks.NewLocalExecHook(hookOpts...)
		jsHooks["local_shell"] = localExecHook
		localHooks["local_shell"] = localExecHook
	}
	hookRepo := hooks.NewPersistentRepo(localHooks, db, http.DefaultClient)
	jsHookRepo := hooks.NewSimpleProvider(jsHooks)

	// 9. Task engine
	exec, err := taskengine.NewExec(ctx, repo, hookRepo, tracker)
	if err != nil {
		return nil, fmt.Errorf("failed to create task executor: %w", err)
	}
	envExec, err := taskengine.NewEnv(ctx, tracker, exec, taskengine.NewSimpleInspector(), hookRepo)
	if err != nil {
		return nil, fmt.Errorf("failed to create environment executor: %w", err)
	}
	envExec, err = taskengine.NewMacroEnv(envExec, hookRepo)
	if err != nil {
		return nil, fmt.Errorf("failed to create macro environment: %w", err)
	}
	taskService := execservice.NewTasksEnv(ctx, envExec, hookRepo)
	execSvc := execservice.NewExec(ctx, repo)
	chainSvc := taskchainservice.New(db)
	functionSvc := functionservice.New(db)
	gojaExec := executor.NewGojaExecutor(tracker, functionSvc)
	dispatcher, err := eventdispatch.New(ctx, functionSvc, func(ctx context.Context, err error) {
		slog.ErrorContext(ctx, "event dispatch error", "error", err)
	}, time.Second, gojaExec, tracker)
	if err != nil {
		return nil, fmt.Errorf("failed to create event dispatcher: %w", err)
	}
	_ = dispatcher
	eventsource := eventsourceservice.NewNoopService()
	gojaExec.AddBuildInServices(eventsource, execSvc, chainSvc, taskService, jsHookRepo)
	gojaExec.StartSync(ctx, time.Second*3)

	jsEnv.SetBuiltinHandlers(jseval.BuiltinHandlers{
		Eventsource:          eventsource,
		TaskService:          execSvc,
		TaskchainService:     chainSvc,
		TaskchainExecService: taskService,
		FunctionService:      functionSvc,
		HookRepo:             jsHookRepo,
	})

	engine.TaskService = taskService
	engine.JSEnv = jsEnv

	oldStop := engine.Stop
	engine.Stop = func() {
		gojaExec.StopSync()
		oldStop()
	}
	return engine, nil
}

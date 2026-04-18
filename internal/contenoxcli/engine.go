package contenoxcli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"

	"github.com/contenox/contenox/execservice"
	"github.com/contenox/contenox/hitlservice"
	"github.com/contenox/contenox/internal/hooks"
	"github.com/contenox/contenox/internal/llmrepo"
	"github.com/contenox/contenox/internal/ollamatokenizer"
	"github.com/contenox/contenox/internal/runtimestate"
	"github.com/contenox/contenox/internal/setupcheck"
	libbus "github.com/contenox/contenox/libbus"
	"github.com/contenox/contenox/libdbexec"
	"github.com/contenox/contenox/libkvstore"
	"github.com/contenox/contenox/libtracker"
	"github.com/contenox/contenox/localhooks"
	"github.com/contenox/contenox/mcpworker"
	"github.com/contenox/contenox/planstore"
	"github.com/contenox/contenox/runtimetypes"
	"github.com/contenox/contenox/stateservice"
	"github.com/contenox/contenox/taskengine"
	"github.com/contenox/contenox/vfsservice"
)

type Engine struct {
	TaskService execservice.TasksEnvService
	Tracker     libtracker.ActivityTracker
	Stop        func()
	Bus         libbus.Messenger
	MCPManager  *mcpworker.Manager
	// LocalHooks lists the names of all registered local hook handlers.
	LocalHooks []string
	// SetupCheck is the last SetupStatus evaluation after RunBackendCycle (for resolver-failure hints).
	SetupCheck setupcheck.Result
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
	}, Bus: bus}

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
	stateOpts = append(stateOpts, runtimestate.WithKVStore(kvMgr), runtimestate.WithAutoDiscoverModels())
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

	// 4b. Keep an internal row for the effective default model so bootstrap groups
	// and local overrides keep working, even though OSS no longer exposes model CRUD.
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
	if !opts.EffectiveSkipBackendCycle {
		if opts.EffectiveTracing {
			slog.Info("Running backend cycle to sync models...")
		}
		if err := state.RunBackendCycle(ctx); err != nil {
			slog.Warn("Backend cycle encountered errors", "error", err)
		}
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

	ss := stateservice.New(state, db)
	res, err := ss.SetupStatus(ctx)
	if err != nil {
		slog.Debug("setup status failed", "error", err)
	} else {
		engine.SetupCheck = res
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
	localHooks := map[string]taskengine.HookRepo{
		"echo":         localhooks.NewEchoHook(),
		"print":        localhooks.NewPrint(tracker),
		"webhook":      localhooks.NewWebCaller(),
		"local_fs":     localhooks.NewLocalFSHook(opts.EffectiveLocalExecAllowedDir),
		"plan_summary": localhooks.NewPlanSummaryHook(planstore.New(db.WithoutTransaction())),
	}
	jsHooks := map[string]taskengine.HookRepo{
		"echo":    localhooks.NewEchoHook(),
		"print":   localhooks.NewPrint(tracker),
		"webhook": localhooks.NewWebCaller(),
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

	// Wrap with HITL interceptor when --hitl is requested.
	if opts.EffectiveHITL {
		hitlVFS := vfsservice.NewLocalFS(opts.ContenoxDir)
		if err := ensureHITLPolicies(opts.ContenoxDir); err != nil {
			slog.Warn("hitl: failed to write embedded policy presets", "error", err)
		}
		hitlSvc := hitlservice.New(hitlVFS, store, tracker)
		hookRepo = localhooks.NewHITLWrapper(hookRepo, NewCLIAskApproval(os.Stderr), hitlSvc, tracker)
	}

	// 9. Task engine
	taskEngineCtx := taskengine.WithTaskEventSink(engineCtx, taskengine.NewBusTaskEventSink(bus))
	exec, err := taskengine.NewExec(taskEngineCtx, repo, hookRepo, tracker)
	if err != nil {
		return nil, fmt.Errorf("failed to create task executor: %w", err)
	}
	envExec, err := taskengine.NewEnv(taskEngineCtx, tracker, exec, taskengine.NewSimpleInspector(), hookRepo)
	if err != nil {
		return nil, fmt.Errorf("failed to create environment executor: %w", err)
	}
	envExec, err = taskengine.NewMacroEnv(envExec, hookRepo)
	if err != nil {
		return nil, fmt.Errorf("failed to create macro environment: %w", err)
	}
	taskService := execservice.NewTasksEnv(engineCtx, envExec, hookRepo)

	engine.TaskService = taskService
	engine.Tracker = tracker

	oldStop := engine.Stop
	engine.Stop = func() {
		mgr.StopAll() // terminates all stdio MCP child processes
		oldStop()
	}
	success = true
	return engine, nil
}

var errTaskEventsRequireRequestID = errors.New("request id is required for task event subscriptions")

// WatchTaskEvents subscribes to request-scoped taskengine events and decodes them
// into structured TaskEvent values for CLI consumers.
func (e *Engine) WatchTaskEvents(ctx context.Context, requestID string, ch chan<- taskengine.TaskEvent) (libbus.Subscription, error) {
	if e == nil || e.Bus == nil {
		return nil, fmt.Errorf("task event bus unavailable")
	}
	requestID = strings.TrimSpace(requestID)
	if requestID == "" {
		return nil, errTaskEventsRequireRequestID
	}

	rawCh := make(chan []byte, 32)
	sub, err := e.Bus.Stream(ctx, taskengine.TaskEventRequestSubject(requestID), rawCh)
	if err != nil {
		return nil, err
	}

	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case payload, ok := <-rawCh:
				if !ok {
					return
				}
				var event taskengine.TaskEvent
				if err := json.Unmarshal(payload, &event); err != nil {
					slog.Warn("failed to decode task event", "error", err)
					continue
				}
				select {
				case ch <- event:
				case <-ctx.Done():
					return
				}
			}
		}
	}()

	return sub, nil
}

// run.go contains the main execution pipeline for the vibe CLI (steps 1–12).
package vibecli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/contenox/vibe/backendservice"
	"github.com/contenox/vibe/eventsourceservice"
	"github.com/contenox/vibe/executor"
	"github.com/contenox/vibe/execservice"
	"github.com/contenox/vibe/functionservice"
	"github.com/contenox/vibe/internal/eventdispatch"
	"github.com/contenox/vibe/internal/hooks"
	"github.com/contenox/vibe/internal/llmrepo"
	"github.com/contenox/vibe/internal/ollamatokenizer"
	"github.com/contenox/vibe/internal/runtimestate"
	"github.com/contenox/vibe/jseval"
	libbus "github.com/contenox/vibe/libbus"
	libdb "github.com/contenox/vibe/libdbexec"
	"github.com/contenox/vibe/libtracker"
	"github.com/contenox/vibe/localhooks"
	"github.com/contenox/vibe/runtimetypes"
	"github.com/contenox/vibe/taskchainservice"
	"github.com/contenox/vibe/taskengine"
)

// runOpts carries all effective config and flags needed by the run pipeline.
type runOpts struct {
	EffectiveDB                       string
	EffectiveChain                    string
	EffectiveDefaultModel             string
	EffectiveDefaultProvider          string
	EffectiveContext                  int
	EffectiveNoDeleteModels           bool
	EffectiveEnableLocalExec          bool
	EffectiveLocalExecAllowedDir      string
	EffectiveLocalExecAllowedCommands string
	EffectiveLocalExecDeniedCommands  []string
	EffectiveTracing                  bool
	EffectiveSteps                    bool
	EffectiveRaw                      bool
	InputValue                        string
	InputFlagPassed                   bool
	Cfg                               localConfig
	ResolvedBackends                  []resolvedBackend
	ContenoxDir                       string
}

func run(ctx context.Context, opts runOpts) {
	// ------------------------------------------------------------------------
	// 1. SQLite database
	// ------------------------------------------------------------------------
	dbPathAbs, err := filepath.Abs(opts.EffectiveDB)
	if err != nil {
		slog.Error("Invalid database path", "error", err)
		os.Exit(1)
	}
	if err := os.MkdirAll(filepath.Dir(dbPathAbs), 0755); err != nil {
		slog.Error("Cannot create database directory", "error", err)
		os.Exit(1)
	}
	db, err := libdb.NewSQLiteDBManager(ctx, dbPathAbs, runtimetypes.SchemaSQLite)
	if err != nil {
		slog.Error("Failed to open SQLite database", "error", err)
		os.Exit(1)
	}
	defer func() {
		if err := db.Close(); err != nil {
			slog.Error("Error closing database", "error", err)
		}
	}()

	// ------------------------------------------------------------------------
	// 2. In-memory bus
	// ------------------------------------------------------------------------
	bus := libbus.NewInMem()
	defer bus.Close()

	// ------------------------------------------------------------------------
	// 3. Runtime state
	// ------------------------------------------------------------------------
	stateOpts := []runtimestate.Option{}
	if opts.EffectiveNoDeleteModels {
		stateOpts = append(stateOpts, runtimestate.WithSkipDeleteUndeclaredModels())
	}
	state, err := runtimestate.New(ctx, db, bus, stateOpts...)
	if err != nil {
		slog.Error("Failed to create runtime state", "error", err)
		os.Exit(1)
	}

	// ------------------------------------------------------------------------
	// 4. Initialize embed/task/chat groups
	// ------------------------------------------------------------------------
	config := &runtimestate.Config{
		TenantID:   localTenantID,
		EmbedModel: opts.EffectiveDefaultModel,
		TaskModel:  opts.EffectiveDefaultModel,
		ChatModel:  opts.EffectiveDefaultModel,
	}
	if err := runtimestate.InitEmbeder(ctx, config, db, opts.EffectiveContext, state); err != nil {
		slog.Error("Failed to init embedder", "error", err)
		os.Exit(1)
	}
	if err := runtimestate.InitPromptExec(ctx, config, db, state, opts.EffectiveContext); err != nil {
		slog.Error("Failed to init prompt executor", "error", err)
		os.Exit(1)
	}
	if err := runtimestate.InitChatExec(ctx, config, db, state, opts.EffectiveContext); err != nil {
		slog.Error("Failed to init chat executor", "error", err)
		os.Exit(1)
	}

	// ------------------------------------------------------------------------
	// 4b. Ensure extra models from config
	// ------------------------------------------------------------------------
	if len(opts.Cfg.ExtraModels) > 0 {
		specs := make([]runtimestate.ExtraModelSpec, 0, len(opts.Cfg.ExtraModels))
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
		if len(specs) > 0 {
			if err := runtimestate.EnsureModels(ctx, db, localTenantID, specs); err != nil {
				slog.Error("Failed to ensure extra models", "error", err)
				os.Exit(1)
			}
		}
	}

	// ------------------------------------------------------------------------
	// 5. Ensure backends from config
	// ------------------------------------------------------------------------
	backendSvc := backendservice.New(db)
	if err := ensureBackendsFromConfig(ctx, db, backendSvc, opts.ResolvedBackends); err != nil {
		slog.Error("Failed to ensure backends", "error", err)
		os.Exit(1)
	}

	// ------------------------------------------------------------------------
	// 6. Run one backend cycle to sync models
	// ------------------------------------------------------------------------
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
			if opts.EffectiveTracing {
				slog.Info("Backend reachable", "id", id, "url", bs.Backend.BaseURL, "models", len(bs.PulledModels))
				for _, m := range bs.PulledModels {
					slog.Debug("Pulled model", "model", m.Model)
				}
			}
		}
	}
	if !anyReachable && opts.EffectiveTracing {
		slog.Warn("No reachable backends – subsequent model operations may fail")
	}

	// ------------------------------------------------------------------------
	// 7. Tokenizer and model manager
	// ------------------------------------------------------------------------
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
		slog.Error("Failed to create model manager", "error", err)
		os.Exit(1)
	}

	// ------------------------------------------------------------------------
	// 8. Local hooks
	// ------------------------------------------------------------------------
	jsEnv := jseval.NewEnv(tracker, jseval.BuiltinHandlers{}, jseval.DefaultBuiltins())
	localHooks := map[string]taskengine.HookRepo{
		// "echo":       localhooks.NewEchoHook(),
		// "print":      localhooks.NewPrint(tracker),
		// "webhook":    localhooks.NewWebCaller(),
		"js_execution": localhooks.NewJSSandboxHook(jsEnv, tracker),
	}
	jsHooks := map[string]taskengine.HookRepo{
		"echo":       localhooks.NewEchoHook(),
		"print":      localhooks.NewPrint(tracker),
		"webhook":    localhooks.NewWebCaller(),
		"js_execution": localhooks.NewJSSandboxHook(jsEnv, tracker),
	}
	if sshHook, err := localhooks.NewSSHHook(); err != nil {
		slog.Debug("SSH hook not registered (e.g. no known_hosts)", "error", err)
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
		jsHooks["local_shell"] = localhooks.NewLocalExecHook(hookOpts...)
	}
	hookRepo := hooks.NewPersistentRepo(localHooks, db, http.DefaultClient)
	jsHookRepo := hooks.NewSimpleProvider(jsHooks)

	// ------------------------------------------------------------------------
	// 9. Task engine
	// ------------------------------------------------------------------------
	exec, err := taskengine.NewExec(ctx, repo, hookRepo, tracker)
	if err != nil {
		slog.Error("Failed to create task executor", "error", err)
		os.Exit(1)
	}
	envExec, err := taskengine.NewEnv(ctx, tracker, exec, taskengine.NewSimpleInspector(), hookRepo)
	if err != nil {
		slog.Error("Failed to create environment executor", "error", err)
		os.Exit(1)
	}
	envExec, err = taskengine.NewMacroEnv(envExec, hookRepo)
	if err != nil {
		slog.Error("Failed to create macro environment", "error", err)
		os.Exit(1)
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
		slog.Error("Failed to create event dispatcher", "error", err)
		os.Exit(1)
	}
	_ = dispatcher // not used with no-op event source; kept for consistent wiring
	// Use no-op event source: CLI does not persist events (SQLite has no partition support).
	eventsource := eventsourceservice.NewNoopService()
	gojaExec.AddBuildInServices(eventsource, execSvc, chainSvc, taskService, jsHookRepo)
	gojaExec.StartSync(ctx, time.Second*3)
	defer gojaExec.StopSync()
	jsEnv.SetBuiltinHandlers(jseval.BuiltinHandlers{
		Eventsource:          eventsource,
		TaskService:         execSvc,
		TaskchainService:    chainSvc,
		TaskchainExecService: taskService,
		FunctionService:     functionSvc,
		HookRepo:            jsHookRepo,
	})
	// ------------------------------------------------------------------------
	// 10. Load chain from file
	// ------------------------------------------------------------------------
	chainPathAbs, err := filepath.Abs(opts.EffectiveChain)
	if err != nil {
		slog.Error("Invalid chain path", "error", err)
		os.Exit(1)
	}
	chainData, err := os.ReadFile(chainPathAbs)
	if err != nil {
		slog.Error("Failed to read chain file", "path", chainPathAbs, "error", err)
		os.Exit(1)
	}
	var chain taskengine.TaskChainDefinition
	if err := json.Unmarshal(chainData, &chain); err != nil {
		slog.Error("Failed to parse chain JSON", "error", err)
		os.Exit(1)
	}

	// Determine input: from flag or stdin if piped
	in := opts.InputValue
	if in == "" && !opts.InputFlagPassed {
		stat, _ := os.Stdin.Stat()
		if (stat.Mode() & os.ModeCharDevice) == 0 {
			bytes, err := io.ReadAll(os.Stdin)
			if err != nil {
				slog.Error("Failed to read from stdin", "error", err)
				os.Exit(1)
			}
			in = string(bytes)
		}
	}
	if in == "" {
		slog.Error("No input for chain", "hint", "pass input as args (e.g. vibe hello), or --input \"your prompt\", or pipe (e.g. echo 'hello' | vibe)")
		os.Exit(1)
	}

	// ------------------------------------------------------------------------
	// 11. Execute chain
	// ------------------------------------------------------------------------
	templateVars := map[string]string{
		"model":    opts.EffectiveDefaultModel,
		"provider": opts.EffectiveDefaultProvider,
		"chain":    chain.ID,
	}
	if builtins := jsEnv.GetBuiltinSignatures(); len(builtins) > 0 {
		var b strings.Builder
		for _, t := range builtins {
			if t.Function.Name != "console" {
				b.WriteString(t.Function.Name)
				b.WriteString(": ")
				b.WriteString(t.Function.Description)
				b.WriteString("\n")
			}
		}
		if hookTools, err := jsEnv.GetExecuteHookToolDescriptions(ctx); err == nil && len(hookTools) > 0 {
			b.WriteString("executeHook tools: ")
			for i, t := range hookTools {
				if i > 0 {
					b.WriteString(", ")
				}
				b.WriteString(t.Function.Name)
			}
		}
		templateVars["sandbox_api"] = b.String()
	}
	for _, key := range opts.Cfg.TemplateVarsFromEnv {
		if v := os.Getenv(key); v != "" {
			templateVars[key] = v
		}
	}
	ctx = taskengine.WithTemplateVars(ctx, templateVars)

	chainInput := taskengine.ChatHistory{
		Messages: []taskengine.Message{{Role: "user", Content: in}},
	}
	if opts.EffectiveTracing {
		slog.Info("Executing chain", "chain", chainPathAbs)
	} else {
		fmt.Fprintln(os.Stderr, "Thinking...")
	}
	output, outputType, stateUnits, err := taskService.Execute(ctx, &chain, chainInput, taskengine.DataTypeChatHistory)
	if err != nil {
		slog.Error("Chain execution failed", "error", err)
		os.Exit(1)
	}

	// ------------------------------------------------------------------------
	// 12. Print results
	// ------------------------------------------------------------------------
	fmt.Println("\n━━━━━━━━━━━━━━━━━━━━")
	printRelevantOutput(output, outputType, opts.EffectiveRaw)
	fmt.Println("━━━━━━━━━━━━━━━━━━━━")
	if opts.EffectiveSteps && len(stateUnits) > 0 {
		fmt.Println("━━━━━━━━━━━━━━━━━━━━")
		fmt.Println("📋 Steps:")
		for i, u := range stateUnits {
			fmt.Printf("  %d. %s (%s) %s %s\n", i+1, u.TaskID, u.TaskHandler, formatDuration(u.Duration), u.Transition)
		}
		fmt.Println("━━━━━━━━━━━━━━━━━━━━")
	}
}

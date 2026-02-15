package main

import (
	"context"
	"fmt"
	"log"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/contenox/vibe/apiframework"
	"github.com/contenox/vibe/embedservice"
	"github.com/contenox/vibe/eventsourceservice"
	"github.com/contenox/vibe/eventstore"
	"github.com/contenox/vibe/execservice"
	"github.com/contenox/vibe/executor"
	"github.com/contenox/vibe/functionservice"
	"github.com/contenox/vibe/functionstore"
	"github.com/contenox/vibe/internal/eventdispatch"
	"github.com/contenox/vibe/internal/hooks"
	"github.com/contenox/vibe/internal/llmrepo"
	"github.com/contenox/vibe/internal/ollamatokenizer"
	"github.com/contenox/vibe/internal/runtimestate"
	"github.com/contenox/vibe/jseval"
	libbus "github.com/contenox/vibe/libbus"
	libdb "github.com/contenox/vibe/libdbexec"
	libroutine "github.com/contenox/vibe/libroutine"
	"github.com/contenox/vibe/libtracker"
	"github.com/contenox/vibe/localhooks"
	"github.com/contenox/vibe/runtimetypes"
	"github.com/contenox/vibe/serverapi"
	"github.com/contenox/vibe/taskchainservice"
	"github.com/contenox/vibe/taskengine"
	"github.com/google/uuid"
)

var (
	cliSetTenancy  string
	Tenancy        = "96ed1c59-ffc1-4545-b3c3-191079c68d79"
	nodeInstanceID = "NODE-Instance-UNSET-dev"
)

func initDatabase(ctx context.Context, cfg *serverapi.Config) (libdb.DBManager, error) {
	dbURL := cfg.DatabaseURL
	var err error
	if dbURL == "" {
		err = fmt.Errorf("DATABASE_URL is required")
		return nil, fmt.Errorf("failed to create store: %w", err)
	}
	var dbInstance libdb.DBManager
	err = libroutine.NewRoutine(10, time.Minute).ExecuteWithRetry(ctx, time.Second, 3, func(ctx context.Context) error {
		dbInstance, err = libdb.NewPostgresDBManager(ctx, dbURL, runtimetypes.Schema)
		if err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("failed to create store: %w", err)
	}

	return dbInstance, nil
}

func initPubSub(ctx context.Context, cfg *serverapi.Config) (libbus.Messenger, error) {
	ps, err := libbus.NewPubSub(ctx, &libbus.Config{
		NATSURL:      cfg.NATSURL,
		NATSPassword: cfg.NATSPassword,
		NATSUser:     cfg.NATSUser,
	})
	if err != nil {
		return nil, err
	}
	return ps, nil
}

func main() {
	if cliSetTenancy == "" {
		log.Fatalf("corrupted build! cliSetTenantID was not injected")
	}

	nodeInstanceID = uuid.NewString()[0:8]
	Tenancy = cliSetTenancy
	config := &serverapi.Config{}
	if err := serverapi.LoadConfig(config); err != nil {
		log.Fatalf("%s: failed to load configuration: %v", nodeInstanceID, err)
	}
	ctx := context.TODO()
	cleanups := []func() error{func() error {
		fmt.Printf("%s cleaning up", nodeInstanceID)
		return nil
	}}
	defer func() {
		for _, cleanup := range cleanups {
			err := cleanup()
			if err != nil {
				log.Printf("%s cleanup failed: %v", nodeInstanceID, err)
			}
		}
	}()
	fmt.Print("initialize the database")
	dbInstance, err := initDatabase(ctx, config)
	if err != nil {
		log.Fatalf("%s initializing database failed: %v", nodeInstanceID, err)
	}
	defer dbInstance.Close()

	ps, err := initPubSub(ctx, config)
	if err != nil {
		log.Fatalf("%s initializing PubSub failed: %v", nodeInstanceID, err)
	}
	if err != nil {
		log.Fatalf("%s initializing OpenSearch failed: %v", nodeInstanceID, err)
	}
	state, err := runtimestate.New(ctx, dbInstance, ps, runtimestate.WithGroups())
	// state, err := runtimestate.New(ctx, dbInstance, ps)
	if err != nil {
		log.Fatalf("%s initializing runtime state failed: %v", nodeInstanceID, err)
	}
	cl, err := strconv.Atoi(config.EmbedModelContextLength)
	if err != nil {
		log.Fatalf("%s parsing embed model context length failed: %v", nodeInstanceID, err)
	}
	err = runtimestate.InitEmbeder(ctx, &runtimestate.Config{
		DatabaseURL: config.DatabaseURL,
		EmbedModel:  config.EmbedModel,
		TaskModel:   config.TaskModel,
		TenantID:    Tenancy,
	}, dbInstance, cl, state)
	if err != nil {
		log.Fatalf("%s initializing embedding group failed: %v", nodeInstanceID, err)
	}
	tokenizerSvc, cleanup, err := ollamatokenizer.NewHTTPClient(ctx, ollamatokenizer.ConfigHTTP{
		BaseURL: config.TokenizerServiceURL,
	})
	if err != nil {
		cleanup()
		log.Fatalf("%s initializing tokenizer service failed: %v", nodeInstanceID, err)
	}
	tcl, err := strconv.Atoi(config.TaskModelContextLength)
	if err != nil {
		log.Fatalf("%s parsing task model context length failed: %v", nodeInstanceID, err)
	}
	err = runtimestate.InitPromptExec(ctx, &runtimestate.Config{
		DatabaseURL: config.DatabaseURL,
		TaskModel:   config.TaskModel,
		EmbedModel:  config.EmbedModel,
		TenantID:    Tenancy,
	}, dbInstance, state, tcl)
	if err != nil {
		log.Fatalf("%s initializing promptexec failed: %v", nodeInstanceID, err)
	}
	tcl, err = strconv.Atoi(config.ChatModelContextLength)
	if err != nil {
		log.Fatalf("%s parsing chat model context length failed: %v", nodeInstanceID, err)
	}
	err = runtimestate.InitChatExec(ctx, &runtimestate.Config{
		DatabaseURL: config.DatabaseURL,
		ChatModel:   config.ChatModel,
		TenantID:    Tenancy,
	}, dbInstance, state, tcl)
	if err != nil {
		log.Fatalf("%s initializing task model failed: %v", nodeInstanceID, err)
	}
	cleanups = append(cleanups, cleanup)
	if err != nil {
		log.Fatalf("%s initializing vector store failed: %v", nodeInstanceID, err)
	}

	// tracker := taskengine.NewKVActivityTracker(kvManager)
	stdOuttracker := libtracker.NewLogActivityTracker(slog.Default())
	serveropsChainedTracker := libtracker.ChainedTracker{
		// tracker,
		stdOuttracker,
	}
	repo, err := llmrepo.NewModelManager(state, tokenizerSvc, llmrepo.ModelManagerConfig{
		DefaultPromptModel: llmrepo.ModelConfig{
			Name:     config.TaskModel,
			Provider: config.TaskProvider,
		},
		DefaultEmbeddingModel: llmrepo.ModelConfig{
			Name:     config.EmbedModel,
			Provider: config.EmbedProvider,
		},
		DefaultChatModel: llmrepo.ModelConfig{
			Name:     config.ChatModel,
			Provider: config.ChatProvider,
		},
	}, serveropsChainedTracker)
	if err != nil {
		log.Fatalf("%s initializing llm repo failed: %v", nodeInstanceID, err)
	}
	jsEnv := jseval.NewEnv(serveropsChainedTracker, jseval.BuiltinHandlers{}, jseval.DefaultBuiltins())
	jsHookRepo := localhooks.NewJSSandboxHook(jsEnv, serveropsChainedTracker)
	localHookrepoInstance := map[string]taskengine.HookRepo{}
	localHookrepoInstance["js_execution"] = jsHookRepo
	localHookrepoInstance["echo"] = localhooks.NewEchoHook()
	localHookrepoInstance["print"] = localhooks.NewPrint(serveropsChainedTracker)
	localHookrepoInstance["webhook"] = localhooks.NewWebCaller()
	localHookrepoInstance["ssh"], err = localhooks.NewSSHHook()
	if err != nil {
		log.Fatalf("%s initializing ssh-hook failed: %v", nodeInstanceID, err)
	}
	hookRepo := hooks.NewPersistentRepo(localHookrepoInstance, dbInstance, http.DefaultClient)
	exec, err := taskengine.NewExec(ctx, repo, hookRepo, serveropsChainedTracker)
	if err != nil {
		log.Fatalf("%s initializing task engine engine failed: %v", nodeInstanceID, err)
	}
	environmentExec, err := taskengine.NewEnv(ctx, serveropsChainedTracker, exec, taskengine.NewSimpleInspector(), hookRepo)
	if err != nil {
		log.Fatalf("%s initializing task engine failed: %v", nodeInstanceID, err)
	}
	environmentExec, err = taskengine.NewMacroEnv(environmentExec, hookRepo)
	if err != nil {
		log.Fatalf("%s initializing task engine NewMacroEnv failed: %v", nodeInstanceID, err)
	}

	cleanups = append(cleanups, cleanup)

	err = eventstore.InitSchema(ctx, dbInstance.WithoutTransaction())
	if err != nil {
		log.Fatalf("%s initializing event store schema failed: %v", nodeInstanceID, err)
	}

	err = functionstore.InitSchema(ctx, dbInstance.WithoutTransaction())
	if err != nil {
		log.Fatalf("%s initializing task store schema failed: %v", nodeInstanceID, err)
	}
	internalMux := http.NewServeMux()
	var apiHandler http.Handler = internalMux
	taskService := execservice.NewTasksEnv(ctx, environmentExec, hookRepo)
	embedService := embedservice.New(repo, config.EmbedModel, config.EmbedProvider)
	execService := execservice.NewExec(ctx, repo)
	functionService := functionservice.New(dbInstance)
	functionService = functionservice.WithActivityTracker(functionService, serveropsChainedTracker)
	executorService := executor.NewGojaExecutor(serveropsChainedTracker, functionService)
	eventbus, err := eventdispatch.New(ctx, functionService, func(ctx context.Context, err error) {
		// TODO:
	}, time.Second, executorService, serveropsChainedTracker)
	if err != nil {
		log.Fatalf("failed to initialize event dispatch service: %v", err)
	}

	eventSourceService, err := eventsourceservice.NewEventSourceService(ctx, dbInstance, ps, eventbus)
	if err != nil {
		log.Fatalf("failed to initialize event source service: %v", err)
	}

	eventSourceService = eventsourceservice.WithActivityTracker(eventSourceService, serveropsChainedTracker)
	taskChainService := taskchainservice.New(dbInstance)
	taskChainService = taskchainservice.WithActivityTracker(taskChainService, serveropsChainedTracker)
	jsEnv.SetBuiltinHandlers(jseval.BuiltinHandlers{
		Eventsource:          eventSourceService,
		TaskService:          execService,
		TaskchainService:     taskChainService,
		TaskchainExecService: taskService,
		FunctionService:      functionService,
		HookRepo:             hookRepo,
	})
	cleanup, err = serverapi.New(ctx, internalMux, nodeInstanceID, Tenancy, config, dbInstance, ps, repo, environmentExec, state, hookRepo, hookRepo, taskService, embedService, execService, taskChainService, functionService, eventSourceService, executorService, eventbus)
	cleanups = append(cleanups, cleanup)
	if err != nil {
		log.Fatalf("%s initializing API handler failed: %v", nodeInstanceID, err)
	}
	apiHandler = apiframework.RequestIDMiddleware(apiHandler)
	apiHandler = apiframework.TracingMiddleware(apiHandler)
	if config.Token != "" {
		apiHandler = apiframework.TokenMiddleware(apiHandler)
		apiHandler = apiframework.EnforceToken(config.Token, apiHandler)
	}

	mux := http.NewServeMux()
	mux.Handle("/", apiHandler)
	port := config.Port
	log.Printf("%s %s starting server on :%s", Tenancy, nodeInstanceID, port)
	if err := http.ListenAndServe(config.Addr+":"+port, mux); err != nil {
		log.Fatalf("%s server failed: %v", nodeInstanceID, err)
	}
}

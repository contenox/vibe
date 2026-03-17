package serverapi

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/contenox/contenox/affinitygroupservice"
	"github.com/contenox/contenox/apiframework"
	"github.com/contenox/contenox/backendservice"
	"github.com/contenox/contenox/downloadservice"
	"github.com/contenox/contenox/embedservice"
	"github.com/contenox/contenox/eventbridgeservice"
	"github.com/contenox/contenox/eventmappingservice"
	"github.com/contenox/contenox/eventsourceservice"
	"github.com/contenox/contenox/execservice"
	"github.com/contenox/contenox/executor"
	"github.com/contenox/contenox/functionservice"
	"github.com/contenox/contenox/hookproviderservice"
	"github.com/contenox/contenox/internal/backendapi"
	"github.com/contenox/contenox/internal/chatapi"
	eventbridgeapi "github.com/contenox/contenox/internal/eventbusapi"
	"github.com/contenox/contenox/internal/eventdispatch"
	"github.com/contenox/contenox/internal/eventmappingapi"
	"github.com/contenox/contenox/internal/eventsourceapi"
	"github.com/contenox/contenox/internal/execapi"
	"github.com/contenox/contenox/internal/execsyncapi"
	"github.com/contenox/contenox/internal/functionapi"
	"github.com/contenox/contenox/internal/groupapi"
	"github.com/contenox/contenox/internal/hooksapi"
	"github.com/contenox/contenox/internal/llmrepo"
	"github.com/contenox/contenox/internal/mcpserverapi"
	"github.com/contenox/contenox/internal/planapi"
	"github.com/contenox/contenox/internal/providerapi"
	"github.com/contenox/contenox/internal/runtimestate"
	"github.com/contenox/contenox/internal/taskchainapi"
	"github.com/contenox/contenox/internal/vfsapi"
	libbus "github.com/contenox/contenox/libbus"
	libdb "github.com/contenox/contenox/libdbexec"
	"github.com/contenox/contenox/libroutine"
	"github.com/contenox/contenox/libtracker"
	"github.com/contenox/contenox/mcpserverservice"
	"github.com/contenox/contenox/mcpworker"
	"github.com/contenox/contenox/modelservice"
	"github.com/contenox/contenox/openaichatservice"
	"github.com/contenox/contenox/planservice"
	"github.com/contenox/contenox/providerservice"
	"github.com/contenox/contenox/runtimetypes"
	"github.com/contenox/contenox/stateservice"
	"github.com/contenox/contenox/taskchainservice"
	"github.com/contenox/contenox/taskengine"
	"github.com/contenox/contenox/vfsservice"
)

func New(
	ctx context.Context,
	mux *http.ServeMux,
	nodeInstanceID string,
	tenancy string,
	config *Config,
	dbInstance libdb.DBManager,
	pubsub libbus.Messenger,
	repo llmrepo.ModelRepo,
	environmentExec taskengine.EnvExecutor,
	state *runtimestate.State,
	hookRegistry taskengine.HookProvider,
	hookRepo taskengine.HookRepo,
	taskService execservice.TasksEnvService,
	embedService embedservice.Service,
	execService execservice.ExecService,
	taskChainService taskchainservice.Service,
	functionService functionservice.Service,
	eventSourceService eventsourceservice.Service,
	executorService *executor.GojaExecutor,
	eventbus eventdispatch.TriggerManager,
	// kvManager libkv.KVManager,
) (func() error, error) {
	cleanup := func() error { return nil }
	// tracker := taskengine.NewKVActivityTracker(kvManager)
	stdOuttracker := libtracker.NewLogActivityTracker(slog.Default())
	serveropsChainedTracker := libtracker.ChainedTracker{
		// tracker,
		stdOuttracker,
	}
	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		apiframework.Error(w, r, apiframework.ErrNotFound, apiframework.ListOperation)
	})
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		// OK
	})
	version := apiframework.GetVersion()
	mux.HandleFunc("GET /version", func(w http.ResponseWriter, r *http.Request) {
		apiframework.Encode(w, r, http.StatusOK, apiframework.AboutServer{Version: version, NodeInstanceID: nodeInstanceID, Tenancy: tenancy})
	})
	backendService := backendservice.New(dbInstance)
	backendService = backendservice.WithActivityTracker(backendService, serveropsChainedTracker)
	stateService := stateservice.New(state)
	stateService = stateservice.WithActivityTracker(stateService, serveropsChainedTracker)
	backendapi.AddBackendRoutes(mux, backendService, stateService)
	groupservice := affinitygroupservice.New(dbInstance)
	backendapi.AddStateRoutes(mux, stateService)
	groupapi.AddgroupRoutes(mux, groupservice)
	// Get circuit breaker group instance
	group := libroutine.GetGroup()

	// Start managed loops using the group
	group.StartLoop(
		ctx,
		&libroutine.LoopConfig{
			Key:          "backendCycle",
			Threshold:    3,
			ResetTimeout: 10 * time.Second,
			Interval:     10 * time.Second,
			Operation:    state.RunBackendCycle,
		},
	)

	group.StartLoop(
		ctx,
		&libroutine.LoopConfig{
			Key:          "downloadCycle",
			Threshold:    3,
			ResetTimeout: 10 * time.Second,
			Interval:     10 * time.Second,
			Operation:    state.RunDownloadCycle,
		},
	)

	// Add this after the group loops are started in serverapi.New
	triggerCh := make(chan []byte, 10)
	err := pubsub.Publish(ctx, "trigger_cycle", []byte("trigger"))
	if err != nil {
		log.Fatalf("failed to publish trigger_cycle message: %v", err)
	}
	sub, err := pubsub.Stream(ctx, "trigger_cycle", triggerCh)
	if err != nil {
		log.Fatalf("failed to subscribe to trigger_cycle topic: %v", err)
	}
	go func() {
		defer sub.Unsubscribe()
		for {
			select {
			case <-ctx.Done():
				return
			case _, ok := <-triggerCh:
				if !ok {
					return
				}
				// Force immediate execution of both cycles
				group.ForceUpdate("backendCycle")
				group.ForceUpdate("downloadCycle")
			}
		}
	}()

	downloadService := downloadservice.New(dbInstance, pubsub)
	downloadService = downloadservice.WithActivityTracker(downloadService, serveropsChainedTracker)
	backendapi.AddQueueRoutes(mux, downloadService)
	modelService := modelservice.New(dbInstance, config.EmbedModel)
	modelService = modelservice.WithActivityTracker(modelService, serveropsChainedTracker)
	backendapi.AddModelRoutes(mux, modelService, downloadService)
	execService = execservice.WithActivityTracker(execService, serveropsChainedTracker)
	embedService = embedservice.WithActivityTracker(embedService, serveropsChainedTracker)
	taskchainapi.AddTaskChainRoutes(mux, taskChainService)
	execapi.AddExecRoutes(mux, execService, taskService, embedService)
	providerService := providerservice.New(dbInstance)
	providerService = providerservice.WithActivityTracker(providerService, serveropsChainedTracker)
	providerapi.AddProviderRoutes(mux, providerService)
	hookproviderService := hookproviderservice.New(dbInstance, hookRegistry)
	hookproviderService = hookproviderservice.WithActivityTracker(hookproviderService, serveropsChainedTracker)
	hooksapi.AddRemoteHookRoutes(mux, hookproviderService)

	mcpService := mcpserverservice.New(dbInstance)
	mcpService = mcpserverservice.WithActivityTracker(mcpService, serveropsChainedTracker)

	// Start persistent MCP session workers — one per registered server.
	// Workers Serve() on NATS; PersistentRepo.execMCPHook routes via Request().
	dbStore := runtimetypes.New(dbInstance.WithoutTransaction())
	workerManager, err := mcpworker.New(ctx, dbStore, pubsub, serveropsChainedTracker)
	if err != nil {
		return nil, fmt.Errorf("failed to start MCP worker manager: %w", err)
	}
	if err := workerManager.WatchEvents(ctx); err != nil {
		return nil, fmt.Errorf("failed to watch MCP lifecycle events: %w", err)
	}

	mcpserverapi.AddMCPServerRoutes(mux, mcpService, pubsub)
	chatService := openaichatservice.New(
		taskService,
		taskChainService,
	)
	chatService = openaichatservice.WithActivityTracker(chatService, serveropsChainedTracker)
	chatapi.AddChatRoutes(mux, chatService)
	eventsourceapi.AddEventSourceRoutes(mux, eventSourceService)

	functionapi.AddFunctionRoutes(mux, functionService)

	execsyncapi.AddExecutorRoutes(mux, executorService, eventbus)
	executorService.AddBuildInServices(eventSourceService, execService, taskChainService, taskService, hookRepo)
	executorService.StartSync(ctx, time.Second*3)

	eventMappingService := eventmappingservice.New(dbInstance)

	eventMappingService = eventmappingservice.WithActivityTracker(eventMappingService, serveropsChainedTracker)
	eventmappingapi.AddMappingRoutes(mux, eventMappingService)
	eventbridgeService := eventbridgeservice.New(eventMappingService, eventSourceService, time.Second*3)
	eventbridgeapi.AddEventBridgeRoutes(mux, eventbridgeService)

	vfsSvc := vfsservice.New(dbInstance, vfsservice.Callbacks{})
	vfsSvc = vfsservice.WithActivityTracker(vfsSvc, serveropsChainedTracker)
	vfsapi.AddRoutes(mux, vfsSvc)

	planSvc := planservice.New(dbInstance, taskService, vfsSvc)
	planapi.AddPlanRoutes(mux, planSvc, taskChainService)

	return cleanup, nil
}

type Config struct {
	DatabaseURL             string `json:"database_url"`
	Port                    string `json:"port"`
	Addr                    string `json:"addr"`
	NATSURL                 string `json:"nats_url"`
	NATSUser                string `json:"nats_user"`
	NATSPassword            string `json:"nats_password"`
	TokenizerServiceURL     string `json:"tokenizer_service_url"`
	EmbedModel              string `json:"embed_model"`
	EmbedProvider           string `json:"embed_provider"`
	EmbedModelContextLength string `json:"embed_model_context_length"`
	TaskModel               string `json:"task_model"`
	TaskProvider            string `json:"task_provider"`
	TaskModelContextLength  string `json:"task_model_context_length"`
	ChatModel               string `json:"chat_model"`
	ChatProvider            string `json:"chat_provider"`
	ChatModelContextLength  string `json:"chat_model_context_length"`
	VectorStoreURL          string `json:"vector_store_url"`
	Token                   string `json:"token"`
	UIBaseURL               string `json:"ui_base_url"`
	ValkeyAddr              string `json:"valkey_addr"`
	ValkeyPassword          string `json:"valkey_password"`
}

func LoadConfig[T any](cfg *T) error {
	if cfg == nil {
		return fmt.Errorf("config pointer is nil")
	}
	config := map[string]string{}
	for _, kvPair := range os.Environ() {
		ar := strings.SplitN(kvPair, "=", 2)
		if len(ar) < 2 {
			continue
		}
		key := strings.ToLower(ar[0])
		value := ar[1]
		config[key] = value
	}

	b, err := json.Marshal(config)
	if err != nil {
		return fmt.Errorf("failed to marshal env vars: %w", err)
	}
	err = json.Unmarshal(b, cfg)
	if err != nil {
		return fmt.Errorf("failed to unmarshal into config struct: %w", err)
	}

	return nil
}

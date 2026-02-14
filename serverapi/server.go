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

	"github.com/contenox/vibe/affinitygroupservice"
	"github.com/contenox/vibe/apiframework"
	"github.com/contenox/vibe/backendservice"
	"github.com/contenox/vibe/downloadservice"
	"github.com/contenox/vibe/embedservice"
	"github.com/contenox/vibe/eventbridgeservice"
	"github.com/contenox/vibe/eventmappingservice"
	"github.com/contenox/vibe/eventsourceservice"
	"github.com/contenox/vibe/execservice"
	"github.com/contenox/vibe/executor"
	"github.com/contenox/vibe/functionservice"
	"github.com/contenox/vibe/hookproviderservice"
	"github.com/contenox/vibe/internal/backendapi"
	"github.com/contenox/vibe/internal/chatapi"
	eventbridgeapi "github.com/contenox/vibe/internal/eventbusapi"
	"github.com/contenox/vibe/internal/eventdispatch"
	"github.com/contenox/vibe/internal/eventmappingapi"
	"github.com/contenox/vibe/internal/eventsourceapi"
	"github.com/contenox/vibe/internal/execapi"
	"github.com/contenox/vibe/internal/execsyncapi"
	"github.com/contenox/vibe/internal/functionapi"
	"github.com/contenox/vibe/internal/groupapi"
	"github.com/contenox/vibe/internal/hooksapi"
	"github.com/contenox/vibe/internal/llmrepo"
	"github.com/contenox/vibe/internal/providerapi"
	"github.com/contenox/vibe/internal/runtimestate"
	"github.com/contenox/vibe/internal/taskchainapi"
	libbus "github.com/contenox/vibe/libbus"
	libdb "github.com/contenox/vibe/libdbexec"
	"github.com/contenox/vibe/libroutine"
	"github.com/contenox/vibe/libtracker"
	"github.com/contenox/vibe/modelservice"
	"github.com/contenox/vibe/openaichatservice"
	"github.com/contenox/vibe/providerservice"
	"github.com/contenox/vibe/stateservice"
	"github.com/contenox/vibe/taskchainservice"
	"github.com/contenox/vibe/taskengine"
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

package serverapi

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/contenox/contenox/affinitygroupservice"
	"github.com/contenox/contenox/apiframework"
	"github.com/contenox/contenox/apiframework/middleware"
	"github.com/contenox/contenox/backendservice"
	"github.com/contenox/contenox/chatsessionmodes"
	"github.com/contenox/contenox/embedservice"
	"github.com/contenox/contenox/execservice"
	"github.com/contenox/contenox/hookproviderservice"
	"github.com/contenox/contenox/internal/backendapi"
	"github.com/contenox/contenox/internal/chatapi"
	"github.com/contenox/contenox/internal/execapi"
	"github.com/contenox/contenox/internal/groupapi"
	"github.com/contenox/contenox/internal/hooksapi"
	internalchatapi "github.com/contenox/contenox/internal/internalchatapi"
	"github.com/contenox/contenox/internal/llmrepo"
	"github.com/contenox/contenox/internal/mcpserverapi"
	"github.com/contenox/contenox/internal/planapi"
	"github.com/contenox/contenox/internal/providerapi"
	"github.com/contenox/contenox/internal/runtimestate"
	"github.com/contenox/contenox/internal/setupapi"
	"github.com/contenox/contenox/internal/taskchainapi"
	"github.com/contenox/contenox/internal/taskeventsapi"
	"github.com/contenox/contenox/internal/vfsapi"
	libbus "github.com/contenox/contenox/libbus"
	libdb "github.com/contenox/contenox/libdbexec"
	"github.com/contenox/contenox/libroutine"
	"github.com/contenox/contenox/libtracker"
	"github.com/contenox/contenox/mcpserverservice"
	"github.com/contenox/contenox/mcpworker"
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
	vfsSvc vfsservice.Service,
	auth middleware.AuthZReader,
	// kvManager libkv.KVManager,
) (func() error, error) {
	cleanup := func() error { return nil }
	var stopMCPOnce sync.Once
	// tracker := taskengine.NewKVActivityTracker(kvManager)
	stdOuttracker := libtracker.NewLogActivityTracker(slog.Default())
	serveropsChainedTracker := libtracker.ChainedTracker{
		// tracker,
		stdOuttracker,
	}
	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		apiframework.Error(w, r, apiframework.ErrNotFound, apiframework.ListOperation)
	})
	AddHealthRoutes(mux)
	version := apiframework.GetVersion()
	AddVersionRoutes(mux, version, nodeInstanceID, tenancy)
	backendService := backendservice.New(dbInstance)
	backendService = backendservice.WithActivityTracker(backendService, serveropsChainedTracker)
	stateService := stateservice.New(state, dbInstance)
	stateService = stateservice.WithActivityTracker(stateService, serveropsChainedTracker)
	backendapi.AddBackendRoutes(mux, backendService, stateService)
	groupservice := affinitygroupservice.New(dbInstance)
	backendapi.AddStateRoutes(mux, stateService)
	setupapi.AddSetupRoutes(mux, stateService, auth)
	taskeventsapi.AddRoutes(mux, pubsub, auth)
	groupapi.AddgroupRoutes(mux, groupservice)
	// Get circuit breaker group instance
	group := libroutine.GetGroup()

	// Start the read-only backend refresh loop using the group.
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
	group.ForceUpdate("backendCycle")
	backendapi.AddModelRoutes(mux, stateService)
	execService = execservice.WithActivityTracker(execService, serveropsChainedTracker)
	embedService = embedservice.WithActivityTracker(embedService, serveropsChainedTracker)
	taskchainapi.AddTaskChainRoutes(mux, taskChainService)
	execapi.AddExecRoutes(mux, execService, taskService, embedService)
	providerService := providerservice.New(dbInstance)
	providerService = providerservice.WithActivityTracker(providerService, serveropsChainedTracker)
	providerapi.AddProviderRoutes(mux, providerService)
	hookproviderService := hookproviderservice.New(dbInstance, hookRegistry, serveropsChainedTracker)
	hookproviderService = hookproviderservice.WithActivityTracker(hookproviderService, serveropsChainedTracker)
	hooksapi.AddRemoteHookRoutes(mux, hookproviderService)

	mcpService := mcpserverservice.New(dbInstance, mcpserverservice.WithUIBaseURL(config.UIBaseURL))
	mcpService = mcpserverservice.WithActivityTracker(mcpService, serveropsChainedTracker)

	// Start persistent MCP session workers — one per registered server.
	// Workers Serve() on NATS; PersistentRepo.execMCPHook routes via Request().
	dbStore := runtimetypes.New(dbInstance.WithoutTransaction())
	workerManager, err := mcpworker.New(ctx, dbStore, pubsub, serveropsChainedTracker)
	if err != nil {
		return nil, fmt.Errorf("failed to start MCP worker manager: %w", err)
	}
	if err := workerManager.WatchEvents(ctx); err != nil {
		workerManager.StopAll()
		return nil, fmt.Errorf("failed to watch MCP lifecycle events: %w", err)
	}

	prevCleanup := cleanup
	cleanup = func() error {
		stopMCPOnce.Do(func() { workerManager.StopAll() })
		return prevCleanup()
	}

	mcpserverapi.AddMCPServerRoutes(mux, mcpService, pubsub, auth)
	chatService := openaichatservice.New(
		taskService,
		taskChainService,
	)
	chatService = openaichatservice.WithActivityTracker(chatService, serveropsChainedTracker)
	chatapi.AddChatRoutes(mux, chatService)

	vfsSvc = vfsservice.WithActivityTracker(vfsSvc, serveropsChainedTracker)
	vfsapi.AddRoutes(mux, vfsSvc)

	planSvc := planservice.New(dbInstance, taskService, vfsSvc)
	planapi.AddPlanRoutes(mux, planSvc, taskChainService, taskService, pubsub)

	chatTurnSvc := chatsessionmodes.New(chatsessionmodes.Deps{
		DB:           dbInstance,
		TaskService:  taskService,
		ChainService: taskChainService,
		PlanService:  planSvc,
	})
	internalchatapi.AddChatRoutes(mux, chatTurnSvc, auth)

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

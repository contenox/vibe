package playground

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/contenox/vibe/affinitygroupservice"
	"github.com/contenox/vibe/backendservice"
	"github.com/contenox/vibe/downloadservice"
	"github.com/contenox/vibe/embedservice"
	"github.com/contenox/vibe/eventsourceservice"
	"github.com/contenox/vibe/eventstore"
	"github.com/contenox/vibe/execservice"
	"github.com/contenox/vibe/executor"
	"github.com/contenox/vibe/functionservice"
	"github.com/contenox/vibe/functionstore"
	"github.com/contenox/vibe/hookproviderservice"
	"github.com/contenox/vibe/internal/eventdispatch"
	"github.com/contenox/vibe/internal/hooks"
	"github.com/contenox/vibe/internal/llmrepo"
	"github.com/contenox/vibe/internal/modelrepo"
	"github.com/contenox/vibe/internal/ollamatokenizer"
	"github.com/contenox/vibe/internal/runtimestate"
	"github.com/contenox/vibe/libbus"
	"github.com/contenox/vibe/libdbexec"
	"github.com/contenox/vibe/libroutine"
	"github.com/contenox/vibe/libtracker"
	"github.com/contenox/vibe/modelservice"
	"github.com/contenox/vibe/openaichatservice"
	"github.com/contenox/vibe/providerservice"
	"github.com/contenox/vibe/runtimetypes"
	"github.com/contenox/vibe/stateservice"
	"github.com/contenox/vibe/taskchainservice"
	"github.com/contenox/vibe/taskengine"
)

// Playground provides a fluent API for setting up a test environment.
// Errors are chained, and execution stops on the first failure.
type Playground struct {
	cleanUps                  []func()
	db                        libdbexec.DBManager
	bus                       libbus.Messenger
	state                     *runtimestate.State
	tokenizer                 ollamatokenizer.Tokenizer
	llmRepo                   llmrepo.ModelRepo
	hookrepo                  taskengine.HookRepo
	eventSourceService        eventsourceservice.Service
	eventDispatcher           eventdispatch.Trigger
	functionService           functionservice.Service
	tracker                   libtracker.ActivityTracker
	gojaExecutor              *executor.GojaExecutor
	eventSourceInit           bool
	functionInit              bool
	withgroup                 bool
	routinesStarted           bool
	embeddingsModel           string
	embeddingsModelProvider   string
	embeddingsModelContextLen int
	llmPromptModel            string
	llmPromptModelProvider    string
	llmPromptModelContextLen  int
	llmChatModel              string
	llmChatModelProvider      string
	ollamaBackendName         string
	llmChatModelContextLen    int
	Error                     error
}

// A fixed tenant ID for testing purposes.
const testTenantID = "00000000-0000-0000-0000-000000000000"

// New creates a new Playground instance.
func New() *Playground {
	return &Playground{}
}

// AddCleanUp adds a cleanup function to be called by CleanUp.
func (p *Playground) AddCleanUp(cleanUp func()) {
	p.cleanUps = append(p.cleanUps, cleanUp)
}

// GetError returns the first error that occurred during the setup chain.
func (p *Playground) GetError() error {
	return p.Error
}

// CleanUp runs all registered cleanup functions.
func (p *Playground) CleanUp() {
	// Run cleanups in reverse order of addition.
	for i := len(p.cleanUps) - 1; i >= 0; i-- {
		p.cleanUps[i]()
	}
}

// StartBackgroundRoutines starts the core background processes for backend and download cycles.
func (p *Playground) StartBackgroundRoutines(ctx context.Context) *Playground {
	if p.Error != nil {
		return p
	}
	if p.state == nil {
		p.Error = errors.New("cannot start background routines: runtime state is not initialized")
		return p
	}

	group := libroutine.GetGroup()

	group.StartLoop(
		ctx,
		&libroutine.LoopConfig{
			Key:          "backendCycle",
			Threshold:    3,
			ResetTimeout: 1 * time.Second,
			Interval:     1 * time.Second,
			Operation:    p.state.RunBackendCycle,
		},
	)

	group.StartLoop(
		ctx,
		&libroutine.LoopConfig{
			Key:          "downloadCycle",
			Threshold:    3,
			ResetTimeout: 1 * time.Second,
			Interval:     1 * time.Second,
			Operation:    p.state.RunDownloadCycle,
		},
	)

	// Force an initial run to kick things off immediately in the test environment.
	group.ForceUpdate("backendCycle")
	group.ForceUpdate("downloadCycle")

	p.routinesStarted = true
	return p
}

func (p *Playground) WithFunctionInit(ctx context.Context) *Playground {
	if p.Error != nil {
		return p
	}
	if p.db == nil {
		p.Error = errors.New("cannot init function service: database is not configured")
		return p
	}
	if !p.functionInit {
		err := functionstore.InitSchema(ctx, p.db.WithoutTransaction())
		if err != nil {
			p.Error = err
			return p
		}
		p.functionInit = true
	}
	p.functionService = functionservice.New(p.db)
	return p
}

func (p *Playground) WithFunctionService(ctx context.Context) *Playground {
	if p.Error != nil {
		return p
	}
	if p.db == nil {
		p.Error = errors.New("cannot init function service: database is not configured")
		return p
	}
	if !p.functionInit {
		err := functionstore.InitSchema(ctx, p.db.WithoutTransaction())
		if err != nil {
			p.Error = err
			return p
		}
		p.functionInit = true
	}
	p.functionService = functionservice.New(p.db)
	return p
}

func (p *Playground) WithEventDispatcher(ctx context.Context, onError func(context.Context, error), syncInterval time.Duration) *Playground {
	if p.Error != nil {
		return p
	}
	if p.functionService == nil {
		p.Error = errors.New("cannot init event dispatcher: function service is not configured")
		return p
	}
	if p.tracker == nil {
		p.tracker = libtracker.NoopTracker{}
	}

	if p.gojaExecutor == nil {
		p.Error = fmt.Errorf("cannot init event dispatcher: goja executor is not configured")
		return p
	}

	p.eventDispatcher, p.Error = eventdispatch.New(ctx, p.functionService, onError, syncInterval, p.gojaExecutor, p.tracker)
	return p
}

// WithInternalOllamaEmbedder initializes the internal embedding model and group.
func (p *Playground) WithInternalOllamaEmbedder(ctx context.Context, modelName string, contextLen int) *Playground {
	if p.Error != nil {
		return p
	}
	if p.db == nil {
		p.Error = errors.New("cannot init internal embedder: database is not configured")
		return p
	}
	p.embeddingsModel = modelName
	p.embeddingsModelProvider = "ollama"
	// Store context length
	p.embeddingsModelContextLen = contextLen
	config := &runtimestate.Config{
		EmbedModel: modelName,
		TenantID:   testTenantID,
	}

	err := runtimestate.InitEmbeder(ctx, config, p.db, contextLen, p.state)
	if err != nil {
		p.Error = fmt.Errorf("failed to initialize internal embedder: %w", err)
	}
	return p
}

func (p *Playground) WithInternalChatExecutor(ctx context.Context, modelName string, contextLen int) *Playground {
	if p.Error != nil {
		return p
	}
	if p.db == nil {
		p.Error = errors.New("cannot init internal chat executor: database is not configured")
		return p
	}

	config := &runtimestate.Config{
		ChatModel: modelName,
		TenantID:  testTenantID,
	}
	// Store context length
	p.llmChatModelContextLen = contextLen
	p.llmChatModel = modelName
	p.llmChatModelProvider = "ollama"

	err := runtimestate.InitChatExec(ctx, config, p.db, p.state, contextLen)
	if err != nil {
		p.Error = fmt.Errorf("failed to initialize internal chat executor: %w", err)
	}
	return p
}

// WithInternalPromptExecutor initializes the internal task/prompt model and group.
func (p *Playground) WithInternalPromptExecutor(ctx context.Context, modelName string, contextLen int) *Playground {
	if p.Error != nil {
		return p
	}
	if p.db == nil {
		p.Error = errors.New("cannot init internal prompt executor: database is not configured")
		return p
	}
	if p.tokenizer == nil {
		p.Error = errors.New("cannot init internal prompt executor: tokenizer is not configured")
		return p
	}

	config := &runtimestate.Config{
		TaskModel: modelName,
		TenantID:  testTenantID,
	}
	// Store context length
	p.llmPromptModelContextLen = contextLen
	p.llmPromptModel = modelName
	p.llmPromptModelProvider = "ollama"

	err := runtimestate.InitPromptExec(ctx, config, p.db, p.state, contextLen)
	if err != nil {
		p.Error = fmt.Errorf("failed to initialize internal prompt executor: %w", err)
	}
	return p
}

// WithOpenAIProvider configures an OpenAI provider with the given API key.
func (p *Playground) WithOpenAIProvider(ctx context.Context, apiKey string, replace bool) *Playground {
	if p.Error != nil {
		return p
	}
	providerSvc, err := p.GetProviderService()
	if err != nil {
		p.Error = fmt.Errorf("failed to get provider service: %w", err)
		return p
	}
	config := &runtimestate.ProviderConfig{
		APIKey: apiKey,
		Type:   providerservice.ProviderTypeOpenAI,
	}
	p.Error = providerSvc.SetProviderConfig(ctx, providerservice.ProviderTypeOpenAI, replace, config)
	return p
}

// WithGeminiProvider configures a Gemini provider with the given API key.
func (p *Playground) WithGeminiProvider(ctx context.Context, apiKey string, replace bool) *Playground {
	if p.Error != nil {
		return p
	}
	providerSvc, err := p.GetProviderService()
	if err != nil {
		p.Error = fmt.Errorf("failed to get provider service: %w", err)
		return p
	}
	config := &runtimestate.ProviderConfig{
		APIKey: apiKey,
		Type:   providerservice.ProviderTypeGemini,
	}
	p.Error = providerSvc.SetProviderConfig(ctx, providerservice.ProviderTypeGemini, replace, config)
	return p
}

// WithPostgresTestContainer sets up a test PostgreSQL container and initializes the DB manager.
func (p *Playground) WithPostgresTestContainer(ctx context.Context) *Playground {
	if p.Error != nil {
		return p
	}
	connStr, _, cleanup, err := libdbexec.SetupLocalInstance(ctx, "test", "test", "test")
	if err != nil {
		p.Error = fmt.Errorf("failed to setup postgres test container: %w", err)
		return p
	}
	p.AddCleanUp(cleanup)

	dbManager, err := libdbexec.NewPostgresDBManager(ctx, connStr, runtimetypes.Schema)
	if err != nil {
		p.Error = fmt.Errorf("failed to create postgres db manager: %w", err)
		return p
	}
	p.db = dbManager
	return p
}

// WithNats sets up a test NATS server.
func (p *Playground) WithNats(ctx context.Context) *Playground {
	if p.Error != nil {
		return p
	}
	ps, cleanup, err := libbus.NewTestPubSub()
	if err != nil {
		p.Error = fmt.Errorf("failed to setup nats test server: %w", err)
		return p
	}
	p.AddCleanUp(cleanup)
	p.bus = ps
	return p
}

// WithDefaultEmbeddingsModel sets the default embeddings model and provider.
func (p *Playground) WithDefaultEmbeddingsModel(model string, provider string, contextLength int) *Playground {
	if p.Error != nil {
		return p
	}
	p.embeddingsModel = model
	p.embeddingsModelProvider = provider
	p.embeddingsModelContextLen = contextLength
	return p
}

// WithDefaultPromptModel sets the default prompt model and provider.
func (p *Playground) WithDefaultPromptModel(model string, provider string, contextLength int) *Playground {
	if p.Error != nil {
		return p
	}
	p.llmPromptModel = model
	p.llmPromptModelProvider = provider
	p.llmPromptModelContextLen = contextLength
	return p
}

// WithDefaultChatModel sets the default chat model and provider.
func (p *Playground) WithDefaultChatModel(model string, provider string, contextLength int) *Playground {
	if p.Error != nil {
		return p
	}
	p.llmChatModel = model
	p.llmChatModelProvider = provider
	p.llmChatModelContextLen = contextLength
	return p
}

// WithRuntimeState initializes the runtime state.
func (p *Playground) WithRuntimeState(ctx context.Context, withgroups bool) *Playground {
	if p.Error != nil {
		return p
	}
	if p.db == nil {
		p.Error = errors.New("cannot initialize runtime state: database is not configured")
		return p
	}
	if p.bus == nil {
		p.Error = errors.New("cannot initialize runtime state: message bus is not configured")
		return p
	}

	var state *runtimestate.State
	var err error
	p.withgroup = withgroups
	if withgroups {
		state, err = runtimestate.New(ctx, p.db, p.bus, runtimestate.WithGroups())
	} else {
		state, err = runtimestate.New(ctx, p.db, p.bus)
	}

	if err != nil {
		p.Error = fmt.Errorf("failed to initialize runtime state: %w", err)
		return p
	}
	p.state = state
	return p
}

// WithActivityTracker sets the activity tracker for the playground
func (p *Playground) WithActivityTracker(tracker libtracker.ActivityTracker) *Playground {
	if p.Error != nil {
		return p
	}
	p.tracker = tracker
	return p
}

// WithGojaExecutor initializes and returns a Goja executor
func (p *Playground) WithGojaExecutor(ctx context.Context) *Playground {
	if p.Error != nil {
		return p
	}

	if p.functionService == nil {
		p.Error = errors.New("function service is not configured")
		return p
	}

	// Use NoopTracker if none specified
	if p.tracker == nil {
		p.tracker = libtracker.NoopTracker{}
	}

	// Create the executor
	p.gojaExecutor = executor.NewGojaExecutor(
		p.tracker,
		p.functionService,
	)

	return p
}

// WithGojaExecutor initializes and returns a Goja executor
func (p *Playground) WithGojaExecutorBuildIns(ctx context.Context) *Playground {
	if p.Error != nil {
		return p
	}

	// Get required services
	taskService, err := p.GetExecService(ctx)
	if err != nil {
		p.Error = err
		return p
	}

	taskchainService, err := p.GetTaskChainService()
	if err != nil {
		p.Error = err
		return p
	}

	taskchainExecService, err := p.GetTasksEnvService(ctx)
	if err != nil {
		p.Error = err
		return p
	}

	eventSourceService, err := p.GetEventSourceService()
	if err != nil {
		p.Error = err
		return p
	}

	// Use NoopTracker if none specified
	if p.tracker == nil {
		p.tracker = libtracker.NoopTracker{}
	}
	if p.hookrepo == nil {
		p.Error = fmt.Errorf("p.hookrepo == nil")
		return p
	}
	// Create the executor
	p.gojaExecutor.AddBuildInServices(eventSourceService, taskService, taskchainService, taskchainExecService, p.hookrepo)

	return p
}

// StartGojaExecutorSync starts the background sync for the Goja executor
func (p *Playground) StartGojaExecutorSync(ctx context.Context, syncInterval time.Duration) *Playground {
	if p.Error != nil {
		return p
	}
	if p.gojaExecutor == nil {
		p.Error = errors.New("Goja executor not initialized")
		return p
	}

	p.gojaExecutor.StartSync(ctx, syncInterval)

	// Add cleanup to stop sync
	p.AddCleanUp(func() {
		p.gojaExecutor.StopSync()
	})

	return p
}

// GetGojaExecutor returns the Goja executor instance
func (p *Playground) GetGojaExecutor() (executor.ExecutorManager, error) {
	if p.Error != nil {
		return nil, p.Error
	}
	if p.gojaExecutor == nil {
		return nil, errors.New("Goja executor not initialized")
	}
	return p.gojaExecutor, nil
}

// WithMockHookRegistry sets up a mock hook registry.
func (p *Playground) WithMockHookRegistry() *Playground {
	if p.Error != nil {
		return p
	}
	if p.state == nil {
		p.Error = errors.New("cannot initialize mock hook registry: runtime state is not configured")
		return p
	}
	p.hookrepo = hooks.NewMockHookRegistry()
	return p
}

// WithMockTokenizer sets up a mock tokenizer.
func (p *Playground) WithMockTokenizer() *Playground {
	if p.Error != nil {
		return p
	}
	if p.state == nil {
		p.Error = errors.New("cannot initialize mock tokenizer: runtime state is not configured")
		return p
	}
	p.tokenizer = ollamatokenizer.MockTokenizer{}
	return p
}

// WithLLMRepo initializes the LLM repository.
func (p *Playground) WithLLMRepo() *Playground {
	if p.Error != nil {
		return p
	}
	if p.state == nil {
		p.Error = errors.New("cannot initialize llm repo: runtime state is not configured")
		return p
	}
	if p.tokenizer == nil {
		p.Error = errors.New("cannot initialize llm repo: tokenizer is not configured")
		return p
	}
	if p.tracker == nil {
		p.tracker = libtracker.NoopTracker{}
	}
	var err error
	p.llmRepo, err = llmrepo.NewModelManager(p.state, p.tokenizer, llmrepo.ModelManagerConfig{
		DefaultEmbeddingModel: llmrepo.ModelConfig{
			Name:     p.embeddingsModel,
			Provider: p.embeddingsModelProvider,
		},
		DefaultPromptModel: llmrepo.ModelConfig{
			Name:     p.llmPromptModel,
			Provider: p.llmPromptModelProvider,
		},
		DefaultChatModel: llmrepo.ModelConfig{
			Name:     p.llmChatModel,
			Provider: p.llmChatModelProvider,
		},
	}, p.tracker)
	if err != nil {
		p.Error = fmt.Errorf("failed to create llm repo model manager: %w", err)
		return p
	}
	return p
}

func (p *Playground) GetFunctionService() (functionservice.Service, error) {
	if p.Error != nil {
		return nil, p.Error
	}
	if p.functionService == nil {
		err := fmt.Errorf("function service is not initialized")
		return nil, err
	}
	return p.functionService, nil
}

// WithOllamaBackend sets up an Ollama test instance and registers it as a backend.
func (p *Playground) WithOllamaBackend(ctx context.Context, name, tag string, assignEmbeddingModel, assignTasksModel bool) *Playground {
	if p.Error != nil {
		return p
	}
	uri, _, cleanup, err := modelrepo.SetupOllamaLocalInstance(ctx, tag)
	if err != nil {
		p.Error = fmt.Errorf("failed to setup ollama local instance: %w", err)
		return p
	}
	p.AddCleanUp(cleanup)

	backends, err := p.GetBackendService()
	if err != nil {
		p.Error = fmt.Errorf("failed to get backend service for ollama setup: %w", err)
		return p
	}

	backend := &runtimetypes.Backend{
		Name:    name,
		BaseURL: uri,
		Type:    "ollama",
	}
	if err := backends.Create(ctx, backend); err != nil {
		p.Error = fmt.Errorf("failed to create ollama backend '%s': %w", name, err)
		return p
	}

	if !p.withgroup {
		return p
	}

	group, err := p.GetGroupService()
	if err != nil {
		p.Error = fmt.Errorf("failed to get group service for ollama setup: %w", err)
		return p
	}

	if assignEmbeddingModel {
		if err := group.AssignBackend(ctx, runtimestate.EmbedgroupID, backend.ID); err != nil {
			p.Error = fmt.Errorf("failed to assign ollama backend to embed group: %w", err)
			return p
		}
	}
	if assignTasksModel {
		if err := group.AssignBackend(ctx, runtimestate.TasksgroupID, backend.ID); err != nil {
			p.Error = fmt.Errorf("failed to assign ollama backend to tasks group: %w", err)
			return p
		}
	}
	return p
}

// WithPostgresReal connects to a real PostgreSQL instance using the provided connection string.
func (p *Playground) WithPostgresReal(ctx context.Context, connStr string) *Playground {
	if p.Error != nil {
		return p
	}
	dbManager, err := libdbexec.NewPostgresDBManager(ctx, connStr, runtimetypes.Schema)
	if err != nil {
		p.Error = fmt.Errorf("failed to create postgres db manager: %w", err)
		return p
	}
	p.db = dbManager
	// No cleanup needed for real resources - user manages lifecycle
	return p
}

// WithNatsReal sets up a connection to a real NATS server.
func (p *Playground) WithNatsReal(ctx context.Context, natsURL, natsUser, natsPassword string) *Playground {
	if p.Error != nil {
		return p
	}
	ps, err := libbus.NewPubSub(ctx, &libbus.Config{
		NATSURL:      natsURL,
		NATSUser:     natsUser,
		NATSPassword: natsPassword,
	})
	if err != nil {
		p.Error = fmt.Errorf("failed to setup nats server: %w", err)
		return p
	}
	p.bus = ps
	// No cleanup needed for real resources - user manages lifecycle
	return p
}

// WithTokenizerService sets up the tokenizer service from a real service URL
func (p *Playground) WithTokenizerService(ctx context.Context, tokenizerURL string) *Playground {
	if p.Error != nil {
		return p
	}

	tokenizerSvc, cleanup, err := ollamatokenizer.NewHTTPClient(ctx, ollamatokenizer.ConfigHTTP{
		BaseURL: tokenizerURL,
	})
	if err != nil {
		p.Error = fmt.Errorf("failed to setup tokenizer service: %w", err)
		return p
	}
	wrappedCleanup := func() {
		_ = cleanup()
	}
	p.tokenizer = tokenizerSvc
	p.AddCleanUp(wrappedCleanup)
	return p
}

func (p *Playground) WithEventSourceInit(ctx context.Context) *Playground {
	if p.Error != nil {
		return p
	}

	if p.bus == nil {
		p.Error = errors.New("cannot initialize event source service: message bus is not configured")
		return p
	}
	if p.eventSourceInit == false {
		err := eventstore.InitSchema(ctx, p.db.WithoutTransaction())
		if err != nil {
			p.Error = fmt.Errorf("failed to initialize event source service: %w", err)
			return p
		}
		p.eventSourceInit = true
	}
	return p
}

func (p *Playground) WithEventSourceService(ctx context.Context) *Playground {
	if p.Error != nil {
		return p
	}
	if p.db == nil {
		p.Error = errors.New("cannot initialize event source service: database is not configured")
		return p
	}
	if p.eventSourceInit == false {
		err := eventstore.InitSchema(ctx, p.db.WithoutTransaction())
		if err != nil {
			p.Error = fmt.Errorf("failed to initialize event source service: %w", err)
			return p
		}
		p.eventSourceInit = true
	}
	if p.eventDispatcher == nil {
		p.Error = errors.New("cannot initialize event source service: event dispatcher is not configured")
		return p
	}
	eventSourceService, err := eventsourceservice.NewEventSourceService(ctx, p.db, p.bus, p.eventDispatcher)
	if err != nil {
		p.Error = fmt.Errorf("failed to initialize event source service: %w", err)
		return p
	}
	p.eventSourceService = eventSourceService
	return p
}

// GetBackendService returns a new backend service instance.
func (p *Playground) GetBackendService() (backendservice.Service, error) {
	if p.Error != nil {
		return nil, p.Error
	}
	if p.db == nil {
		return nil, errors.New("cannot get backend service: database is not initialized")
	}
	return backendservice.New(p.db), nil
}

// GetDownloadService returns a new download service instance.
func (p *Playground) GetDownloadService() (downloadservice.Service, error) {
	if p.Error != nil {
		return nil, p.Error
	}
	if p.db == nil {
		return nil, errors.New("cannot get download service: database is not initialized")
	}
	if p.bus == nil {
		return nil, errors.New("cannot get download service: message bus is not initialized")
	}
	return downloadservice.New(p.db, p.bus), nil
}

// GetModelService returns a new model service instance.
func (p *Playground) GetModelService() (modelservice.Service, error) {
	if p.Error != nil {
		return nil, p.Error
	}
	if p.db == nil {
		return nil, errors.New("cannot get model service: database is not initialized")
	}
	return modelservice.New(p.db, p.embeddingsModel), nil
}

// GetGroupService returns a new group service instance.
func (p *Playground) GetGroupService() (affinitygroupservice.Service, error) {
	if p.Error != nil {
		return nil, p.Error
	}
	if p.db == nil {
		return nil, errors.New("cannot get group service: database is not initialized")
	}
	return affinitygroupservice.New(p.db), nil
}

// GetProviderService returns a new provider service instance.
func (p *Playground) GetProviderService() (providerservice.Service, error) {
	if p.Error != nil {
		return nil, p.Error
	}
	if p.db == nil {
		return nil, errors.New("cannot get provider service: database is not initialized")
	}
	return providerservice.New(p.db), nil
}

func (p *Playground) GetEventSourceService() (eventsourceservice.Service, error) {
	if p.Error != nil {
		return nil, p.Error
	}
	if p.eventSourceService == nil {
		return nil, errors.New("event source service is not initialized, call WithEventSourceService first")
	}
	return p.eventSourceService, nil
}

// GetEmbedService returns a new embed service instance.
func (p *Playground) GetEmbedService() (embedservice.Service, error) {
	if p.Error != nil {
		return nil, p.Error
	}
	if p.llmRepo == nil {
		return nil, errors.New("cannot get embed service: llm repo is not initialized")
	}
	if p.embeddingsModel == "" {
		return nil, errors.New("cannot get embed service: embeddings model is not configured")
	}
	if p.embeddingsModelProvider == "" {
		return nil, errors.New("cannot get embed service: embeddings model provider is not configured")
	}
	return embedservice.New(p.llmRepo, p.embeddingsModel, p.embeddingsModelProvider), nil
}

// GetStateService returns a new state service instance.
func (p *Playground) GetStateService() (stateservice.Service, error) {
	if p.Error != nil {
		return nil, p.Error
	}
	if p.state == nil {
		return nil, errors.New("cannot get state service: runtime state is not initialized")
	}
	return stateservice.New(p.state), nil
}

// GetTaskChainService returns a new task chain service instance.
func (p *Playground) GetTaskChainService() (taskchainservice.Service, error) {
	if p.Error != nil {
		return nil, p.Error
	}
	if p.db == nil {
		return nil, errors.New("cannot get task chain service: database is not initialized")
	}
	return taskchainservice.New(p.db), nil
}

// GetExecService returns a new exec service instance.
func (p *Playground) GetExecService(ctx context.Context) (execservice.ExecService, error) {
	if p.Error != nil {
		return nil, p.Error
	}
	if p.llmRepo == nil {
		return nil, errors.New("cannot get exec service: llmRepo is not initialized")
	}
	return execservice.NewExec(ctx, p.llmRepo), nil
}

// GetTasksEnvService returns a new tasks environment service instance.
func (p *Playground) GetTasksEnvService(ctx context.Context) (execservice.TasksEnvService, error) {
	if p.Error != nil {
		return nil, p.Error
	}
	if p.llmRepo == nil {
		return nil, errors.New("cannot get tasks env service: llmRepo is not initialized")
	}
	if p.hookrepo == nil {
		return nil, errors.New("cannot get tasks env service: hookrepo is not initialized")
	}

	exec, err := taskengine.NewExec(ctx, p.llmRepo, p.hookrepo, libtracker.NewLogActivityTracker(slog.Default()))
	if err != nil {
		return nil, fmt.Errorf("failed to create task engine exec: %w", err)
	}

	env, err := taskengine.NewEnv(ctx, libtracker.NewLogActivityTracker(slog.Default()), exec, taskengine.NewSimpleInspector(), p.hookrepo)
	if err != nil {
		return nil, fmt.Errorf("failed to create task engine env: %w", err)
	}

	return execservice.NewTasksEnv(ctx, env, p.hookrepo), nil
}

// GetChatService returns a new chat service instance.
func (p *Playground) GetChatService(ctx context.Context) (openaichatservice.Service, error) {
	if p.Error != nil {
		return nil, p.Error
	}

	envExec, err := p.GetTasksEnvService(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to get tasks env service for chat service: %w", err)
	}

	taskChainService, err := p.GetTaskChainService()
	if err != nil {
		return nil, fmt.Errorf("failed to get task chain service for chat service: %w", err)
	}

	return openaichatservice.New(envExec, taskChainService), nil
}

// GetHookProviderService returns a new hook provider service instance.
func (p *Playground) GetHookProviderService() (hookproviderservice.Service, error) {
	if p.Error != nil {
		return nil, p.Error
	}
	if p.db == nil {
		return nil, errors.New("cannot get hook provider service: database is not initialized")
	}
	if p.hookrepo == nil {
		return nil, errors.New("cannot get hook provider service: hook repository is not initialized")
	}
	return hookproviderservice.New(p.db, p.hookrepo), nil
}

// New method for real Ollama backend (complements container-based)
func (p *Playground) WithOllamaBackendReal(ctx context.Context, name, uri string, assignEmbeddingModel, assignTasksModel bool) *Playground {
	if p.Error != nil {
		return p
	}
	if p.state == nil {
		p.Error = errors.New("cannot setup ollama backend real: runtime state not configured")
		return p
	}

	backends, err := p.GetBackendService()
	if err != nil {
		p.Error = fmt.Errorf("backend service error: %w", err)
		return p
	}

	backend := &runtimetypes.Backend{
		Name:    name,
		BaseURL: uri,
		Type:    "ollama",
	}
	if err := backends.Create(ctx, backend); err != nil {
		p.Error = fmt.Errorf("create backend '%s' failed: %w", name, err)
		return p
	}

	p.ollamaBackendName = name // Track for WaitUntilModelIsReady

	if !p.withgroup {
		return p
	}

	group, err := p.GetGroupService()
	if err != nil {
		p.Error = fmt.Errorf("group service error: %w", err)
		return p
	}

	if assignEmbeddingModel {
		if err := group.AssignBackend(ctx, runtimestate.EmbedgroupID, backend.ID); err != nil {
			p.Error = fmt.Errorf("assign to embed group failed: %w", err)
			return p
		}
	}
	if assignTasksModel {
		if err := group.AssignBackend(ctx, runtimestate.TasksgroupID, backend.ID); err != nil {
			p.Error = fmt.Errorf("assign to tasks group failed: %w", err)
			return p
		}
	}
	return p
}

// WaitUntilModelIsReady blocks until the specified model is available on the specified backend.
func (p *Playground) WaitUntilModelIsReady(ctx context.Context, backendName, modelName string) error {
	if p.Error != nil {
		return p.Error
	}
	if !p.routinesStarted {
		return errors.New("WaitUntilModelIsReady called before WithBackgroundRoutines; routines are not running")
	}

	stateService, err := p.GetStateService()
	if err != nil {
		return fmt.Errorf("could not get state service to wait for model: %w", err)
	}

	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("timed out waiting for model '%s' on backend '%s': %w", modelName, backendName, ctx.Err())

		case <-ticker.C:
			allStates, err := stateService.Get(ctx)
			if err != nil {
				// Log or ignore transient errors and continue trying.
				continue
			}

			for _, backendState := range allStates {
				if backendState.Name == backendName {
					for _, pulledModel := range backendState.PulledModels {
						if pulledModel.Model == modelName {
							// Success! The model is ready.
							return nil
						}
					}
					// Found the backend, but not the model yet. Continue waiting.
					break
				}
			}
		}
	}
}

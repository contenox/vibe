// Package runtimestate reconciles the declared state of LLM backends (from
// dbInstance) with their actual observed state. It keeps runtime observation
// read-only and is intended to be executed repeatedly within background tasks
// managed externally.
//
// The central type is State (constructed via New), which other packages
// — notably llmrepo — consult through ProviderFromRuntimeState to enumerate
// currently-available modelrepo.Provider instances. Init{Embeder,PromptExec,
// ChatExec} bootstrap default model configurations from a Config; provider
// subpackages register their catalogs via blank imports.
package runtimestate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	libbus "github.com/contenox/beam/internal/libbus"
	libdb "github.com/contenox/beam/internal/libdbexec"
	"github.com/contenox/beam/internal/libkvstore"
	"github.com/contenox/beam/internal/models/modelrepo"
	"github.com/contenox/beam/internal/models/statetype"
	"github.com/contenox/beam/internal/store/runtimetypes"
)

// ProviderCacheDuration defines how long the state of models from an external
// provider (like OpenAI or Gemini) is cached to avoid frequent API calls.
const ProviderCacheDuration = 1 * time.Hour

// ReconcileDebounceInterval bounds how often a read-triggered reconcile
// (ReconcileIfStale) actually re-scans backends. The runtime reconciles at
// startup and on explicit refresh only (no periodic loop), so a read view that
// self-heals must debounce or a burst of UI polls would stampede a full cycle
// each time. It is a package var so tests can shorten it.
var ReconcileDebounceInterval = 15 * time.Second

// providerCacheEntry holds the data and metadata for a cached provider state.
// APIKey is stored so we can detect key rotation and invalidate the cache.
type providerCacheEntry struct {
	Models []modelrepo.ObservedModel `json:"models"`
	APIKey string                    `json:"api_key"`
}

// State manages the overall runtime status of multiple LLM backends.
// It orchestrates synchronization between the desired configuration
// and the actual observed state of the backends.
type State struct {
	dbInstance         libdb.DBManager
	state              sync.Map
	psInstance         libbus.Messenger
	withgroups         bool
	autoDiscoverModels bool // when true, expose all live backend models without requiring declaration
	// kvStore is used for persistent provider-model caching (nil = fall back to in-memory sync.Map)
	kvStore       libkvstore.KVManager
	providerCache sync.Map // fallback when kvStore is nil
	// reconcileMu guards lastReconcileAt, the debounce clock shared by every
	// reconcile (RunBackendCycle and the read-triggered ReconcileIfStale).
	reconcileMu     sync.Mutex
	lastReconcileAt time.Time
}

type Option func(*State)

func WithGroups() Option {
	return func(s *State) {
		s.withgroups = true
	}
}

// WithKVStore injects a persistent KV store for provider model-list caching.
// For the CLI use libkvstore.NewSQLiteManager; for the runtime API use libkvstore.NewManager (Valkey).
// When not provided the cache falls back to an in-memory sync.Map.
func WithKVStore(kv libkvstore.KVManager) Option {
	return func(s *State) {
		s.kvStore = kv
	}
}

// WithSkipDeleteUndeclaredModels is kept as a no-op compatibility option.
// OSS runtime reconciliation is observation-only and no longer deletes backend models.
func WithSkipDeleteUndeclaredModels() Option {
	return func(s *State) {}
}

// WithAutoDiscoverModels exposes all models returned by live backends without requiring manual
// declaration via 'model add'. Capability inference remains name-based for providers
// (e.g. OpenAI) whose APIs do not return capability metadata.
func WithAutoDiscoverModels() Option {
	return func(s *State) {
		s.autoDiscoverModels = true
	}
}

// New creates and initializes a new State manager.
// It requires a database manager (dbInstance) to load the desired configurations
// and a messenger instance (psInstance) for event handling and progress updates.
// Options allow enabling experimental features like group-based reconciliation.
// Returns an initialized State ready for use.
func New(ctx context.Context, dbInstance libdb.DBManager, psInstance libbus.Messenger, options ...Option) (*State, error) {
	s := &State{
		dbInstance: dbInstance,
		state:      sync.Map{},
		psInstance: psInstance,
	}
	if psInstance == nil {
		return nil, errors.New("psInstance cannot be nil")
	}
	if dbInstance == nil {
		return nil, errors.New("dbInstance cannot be nil")
	}
	// Apply options to configure the State instance
	for _, option := range options {
		option(s)
	}
	return s, nil
}

// RunBackendCycle performs a single reconciliation check for all configured LLM backends.
// It compares the desired state (from configuration) with the observed state
// (by communicating with the backends) and refreshes the runtime snapshot.
// This method should be called periodically in a background process.
// DESIGN NOTE: This method executes one complete reconciliation cycle and then returns.
// It does not manage its own background execution (e.g., via internal goroutines or timers).
// This deliberate design choice delegates execution management (scheduling, concurrency control,
// lifecycle via context, error handling, circuit breaking, etc.) entirely to the caller.
//
// Consequently, this method should be called periodically by an external process
// responsible for its scheduling and lifecycle.
// When the group feature is enabled via Withgroups option, it uses group-aware reconciliation.
func (s *State) RunBackendCycle(ctx context.Context) error {
	var err error
	if s.withgroups {
		err = s.syncBackendsWithgroups(ctx)
	} else {
		err = s.syncBackends(ctx)
	}
	// Feed the debounce clock so an explicit refresh (or the chat-path reconcile)
	// suppresses a redundant read-triggered cycle right after. Recorded even on
	// error so a persistently-failing backend cannot be polled into a hot loop.
	s.markReconciled(time.Now())
	return err
}

// ReconcileIfStale runs one RunBackendCycle when the last reconcile is older than
// ReconcileDebounceInterval, otherwise it is a no-op. The runtime reconciles at
// startup and on explicit refresh only (no periodic loop), so a backend that
// comes up afterwards — most commonly modeld being (re)started after the runtime
// — would otherwise stay invisible to read-only views (GET /state,
// GET /setup-status) until a restart. Calling this from those read paths lets
// them self-heal, debounced so a burst of UI polls coalesces into one re-scan.
func (s *State) ReconcileIfStale(ctx context.Context) error {
	if !s.claimReconcile(time.Now()) {
		return nil
	}
	return s.RunBackendCycle(ctx)
}

// claimReconcile reports whether a reconcile is due (last one older than
// ReconcileDebounceInterval) and, when it is, claims the window by advancing the
// clock before returning. Claiming under the lock means concurrent callers skip
// instead of stampeding a second cycle. Pure decision + injected clock so the
// debounce is unit-testable without running a real cycle.
func (s *State) claimReconcile(now time.Time) bool {
	s.reconcileMu.Lock()
	defer s.reconcileMu.Unlock()
	if !s.lastReconcileAt.IsZero() && now.Sub(s.lastReconcileAt) < ReconcileDebounceInterval {
		return false
	}
	s.lastReconcileAt = now
	return true
}

func (s *State) markReconciled(now time.Time) {
	s.reconcileMu.Lock()
	s.lastReconcileAt = now
	s.reconcileMu.Unlock()
}

// Get returns a copy of the current observed state for all backends.
// This provides a safe snapshot for reading state without risking modification
// of the internal structures.
func (s *State) Get(ctx context.Context) map[string]statetype.BackendRuntimeState {
	state := map[string]statetype.BackendRuntimeState{}
	s.state.Range(func(key, value any) bool {
		backend, ok := value.(*statetype.BackendRuntimeState)
		if !ok {
			// log.Fatalf("invalid type in state: %T", value)
			return true
		}
		var backendCopy statetype.BackendRuntimeState
		raw, err := json.Marshal(backend)
		if err != nil {
			// log.Fatalf("failed to marshal backend: %v", err)
		}
		err = json.Unmarshal(raw, &backendCopy)
		if err != nil {
			// log.Fatalf("failed to unmarshal backend: %v", err)
		}
		backendCopy.SetAPIKey(backend.GetAPIKey())
		state[backend.ID] = backendCopy
		return true
	})
	return state
}

// cleanupStaleBackends removes state entries for backends not present in currentIDs.
// It performs type checking on state keys and logs errors for invalid key types.
// This centralizes the state cleanup logic used by all reconciliation flows.
func (s *State) cleanupStaleBackends(currentIDs map[string]struct{}) error {
	var err error
	s.state.Range(func(key, value any) bool {
		id, ok := key.(string)
		if !ok {
			err = fmt.Errorf("BUG: invalid key type: %T %v", key, key)
			// log.Printf("BUG: %v", err)
			return true
		}
		if _, exists := currentIDs[id]; !exists {
			s.state.Delete(id)
		}
		return true
	})
	return err
}

// syncBackendsWithgroups is the group-aware reconciliation logic called by RunBackendCycle.
// It:
//  1. Fetches all configured groups from the database.
//  2. For each group:
//     a. Retrieves its associated backends and models.
//     b. Aggregates models for each backend, collecting a unique set of all models
//     that a backend should have based on all groups it belongs to.
//     c. Tracks all active backend IDs encountered.
//  3. After processing all groups and aggregating models:
//     a. For each unique backend, processes it once with its complete aggregated set of models.
//  4. Performs global cleanup of state entries for backends not found in any group (those not
//     associated with any group).
//
// This fixed version aggregates backend IDs across all groups before cleanup to prevent
// premature deletion of valid cross-group backends.
func (s *State) syncBackendsWithgroups(ctx context.Context) error {
	tx := s.dbInstance.WithoutTransaction()
	dbStore := runtimetypes.New(tx)

	allgroups, err := dbStore.ListAllAffinityGroups(ctx)
	if err != nil {
		return fmt.Errorf("fetching groups: %v", err)
	}

	allBackendObjects := make(map[string]*runtimetypes.Backend)
	backendToAggregatedModels := make(map[string]map[string]*runtimetypes.Model)
	activeBackendIDs := make(map[string]struct{})

	for _, group := range allgroups {
		groupBackends, err := dbStore.ListBackendsForAffinityGroup(ctx, group.ID)
		if err != nil {
			return fmt.Errorf("fetching backends for group %s: %v", group.ID, err)
		}

		groupModels, err := dbStore.ListModelsForAffinityGroup(ctx, group.ID)
		if err != nil {
			return fmt.Errorf("fetching models for group %s: %v", group.ID, err)
		}

		for _, backend := range groupBackends {
			activeBackendIDs[backend.ID] = struct{}{}
			if _, exists := allBackendObjects[backend.ID]; !exists {
				allBackendObjects[backend.ID] = backend
			}
			if _, exists := backendToAggregatedModels[backend.ID]; !exists {
				backendToAggregatedModels[backend.ID] = make(map[string]*runtimetypes.Model)
			}
			for _, model := range groupModels {
				backendToAggregatedModels[backend.ID][model.Model] = model
			}
		}
	}

	// Now, process each unique backend once with its fully aggregated list of models.
	for backendID, backendObj := range allBackendObjects {
		modelsForThisBackend := make([]*runtimetypes.Model, 0, len(backendToAggregatedModels[backendID]))
		for _, model := range backendToAggregatedModels[backendID] {
			modelsForThisBackend = append(modelsForThisBackend, model)
		}
		s.processBackend(ctx, backendObj, modelsForThisBackend)
	}

	return s.cleanupStaleBackends(activeBackendIDs)
}

// syncBackends is the global reconciliation logic called by RunBackendCycle.
// It:
// 1. Fetches all configured backends from the database
// 2. Retrieves all models regardless of group association
// 3. Processes each backend with the full model list
// 4. Cleans up state entries for backends no longer present in the database
// This version uses the shared helper methods but maintains its original non-group
// behavior by operating on the global backend/model lists.
func (s *State) syncBackends(ctx context.Context) error {
	tx := s.dbInstance.WithoutTransaction()
	storeInstance := runtimetypes.New(tx)

	backends, err := storeInstance.ListAllBackends(ctx)
	if err != nil {
		return fmt.Errorf("fetching backends: %v", err)
	}

	allModels, err := storeInstance.ListAllModels(ctx)
	if err != nil {
		return fmt.Errorf("fetching paginated models: %v", err)
	}

	currentIDs := make(map[string]struct{})
	s.processBackends(ctx, backends, allModels, currentIDs)
	return s.cleanupStaleBackends(currentIDs)
}

// Helper method to process backends and collect their IDs
func (s *State) processBackends(ctx context.Context, backends []*runtimetypes.Backend, models []*runtimetypes.Model, currentIDs map[string]struct{}) {
	for _, backend := range backends {
		currentIDs[backend.ID] = struct{}{}
		s.processBackend(ctx, backend, models)
	}
}

// processBackend routes the backend processing logic based on the backend's Type.
// It acts as a dispatcher to type-specific handling functions (e.g., for Ollama).
// It updates the internal state map with the results of the processing,
// including any errors encountered for unsupported types.
// Helper method to process backends and collect their IDs
func (s *State) processBackend(ctx context.Context, backend *runtimetypes.Backend, declaredModels []*runtimetypes.Model) {
	switch modelrepo.CanonicalBackendType(backend.Type) {
	case "ollama":
		s.processOllamaBackend(ctx, backend, declaredModels)
	case "vllm":
		s.processVLLMBackend(ctx, backend, declaredModels)
	case "gemini":
		s.processGeminiBackend(ctx, backend, declaredModels)
	case "openai", "openrouter", "anthropic", "mistral":
		// Direct cloud, API-key + OpenAI-style model listing. processOpenAIBackend
		// is generic over backend.Type (keys, catalog), so it serves all of them.
		s.processOpenAIBackend(ctx, backend, declaredModels)
	case "vertex-google":
		s.processVertexBackend(ctx, backend, declaredModels)
	case "bedrock":
		s.processBedrockBackend(ctx, backend, declaredModels)
	default:
		brokenService := &statetype.BackendRuntimeState{
			ID:      backend.ID,
			Name:    backend.Name,
			Models:  []string{},
			Backend: *backend,
			Error:   "Unsupported backend type: " + backend.Type,
		}
		s.state.Store(backend.ID, brokenService)
	}
}

// processOllamaBackend handles runtime observation for a single Ollama backend.
// It lists the models currently exposed by the backend, merges that data with
// declared overrides, and publishes the resulting runtime snapshot.
func (s *State) processOllamaBackend(ctx context.Context, backend *runtimetypes.Backend, declaredOllamaModels []*runtimetypes.Model) {
	models := []string{}
	declaredModelMap := make(map[string]runtimetypes.Model)
	for _, model := range declaredOllamaModels {
		declaredModelMap[model.Model] = *model
		models = append(models, model.Model)
	}

	apiKey := ""
	if key, err := s.loadProviderAPIKey(ctx, backend.Type); err == nil {
		apiKey = key
	}

	catalog, err := s.newCatalogProvider(backend, apiKey)
	if err != nil {
		storeBackendError(s, backend, apiKey, err, models)
		return
	}

	observedModels, err := catalog.ListModels(ctx)
	if err != nil {
		storeBackendError(s, backend, apiKey, err, models)
		return
	}

	stateservice := &statetype.BackendRuntimeState{
		ID:      backend.ID,
		Name:    backend.Name,
		Backend: *backend,
		Models:  make([]string, 0, len(observedModels)),
	}
	stateservice.SetAPIKey(apiKey)

	// Create proper model entries with capabilities.
	pulledModels := make([]statetype.ModelPullStatus, 0, len(observedModels))
	for _, observed := range observedModels {
		lmr := pullStatusFromObservedModel(observed)

		// If the declared model has no context_length yet (auto-detect placeholder),
		// write the discovered value back to the DB so subsequent cycles skip re-learning.
		if decl, exists := declaredModelMap[observed.Name]; exists && decl.ContextLength == 0 && lmr.ContextLength > 0 {
			declCopy := decl
			declCopy.ContextLength = lmr.ContextLength
			declCopy.CanChat = lmr.CanChat
			declCopy.CanEmbed = lmr.CanEmbed
			declCopy.CanPrompt = lmr.CanPrompt
			declCopy.CanStream = lmr.CanStream
			_ = runtimetypes.New(s.dbInstance.WithoutTransaction()).UpdateModel(ctx, &declCopy)
		}

		// Declared caps act as explicit overrides (admin intent wins over observed values).
		if declaredModel, exists := declaredModelMap[observed.Name]; exists {
			if declaredModel.ContextLength > 0 {
				lmr.ContextLength = declaredModel.ContextLength
			}
			if declaredModel.CanChat {
				lmr.CanChat = true
			}
			if declaredModel.CanEmbed {
				lmr.CanEmbed = true
			}
			if declaredModel.CanPrompt {
				lmr.CanPrompt = true
			}
			if declaredModel.CanStream {
				lmr.CanStream = true
			}
		}

		lmr = s.applyCapabilityOverrides(ctx, backend.Type, lmr)
		pulledModels = append(pulledModels, lmr)
	}

	stateservice.PulledModels = pulledModels
	if s.autoDiscoverModels {
		stateservice.Models = observedModelNames(observedModels)
	} else {
		stateservice.Models = models
	}
	s.state.Store(backend.ID, stateservice)
}

// processVLLMBackend handles the state reconciliation for a single vLLM backend.
// Since vLLM instances typically serve a single model, we verify that the running model
// matches one of the models assigned to the backend through its groups.
func (s *State) processVLLMBackend(ctx context.Context, backend *runtimetypes.Backend, models []*runtimetypes.Model) {
	declaredModelMap := make(map[string]*runtimetypes.Model)
	for _, m := range models {
		declaredModelMap[m.Model] = m
	}
	catalog, err := s.newCatalogProvider(backend, "")
	if err != nil {
		storeBackendError(s, backend, "", err, nil)
		return
	}

	observedModels, err := catalog.ListModels(ctx)
	if err != nil {
		storeBackendError(s, backend, "", err, nil)
		return
	}
	if len(observedModels) == 0 {
		storeBackendError(s, backend, "", fmt.Errorf("No models found in response"), nil)
		return
	}

	res := &statetype.BackendRuntimeState{
		ID:      backend.ID,
		Name:    backend.Name,
		Models:  observedModelNames(observedModels),
		Backend: *backend,
	}

	pulledModels := make([]statetype.ModelPullStatus, 0, len(observedModels))
	for _, observed := range observedModels {
		if declaredModel, exists := declaredModelMap[observed.Name]; exists {
			effectiveContextLen := declaredModel.ContextLength
			if effectiveContextLen == 0 && observed.ContextLength > 0 {
				effectiveContextLen = observed.ContextLength
				declCopy := *declaredModel
				declCopy.ContextLength = observed.ContextLength
				_ = runtimetypes.New(s.dbInstance.WithoutTransaction()).UpdateModel(ctx, &declCopy)
			}

			lmr := statetype.ModelPullStatus{
				Name:            declaredModel.ID,
				Model:           declaredModel.Model,
				ModifiedAt:      declaredModel.UpdatedAt,
				ContextLength:   effectiveContextLen,
				MaxOutputTokens: observed.MaxOutputTokens,
				CanChat:         declaredModel.CanChat,
				CanEmbed:        declaredModel.CanEmbed,
				CanPrompt:       declaredModel.CanPrompt,
				CanStream:       declaredModel.CanStream,
			}
			lmr = s.applyCapabilityOverrides(ctx, backend.Type, lmr)
			pulledModels = append(pulledModels, lmr)
			continue
		}

		if s.autoDiscoverModels {
			lmr := s.applyCapabilityOverrides(ctx, backend.Type, pullStatusFromObservedModel(observed))
			pulledModels = append(pulledModels, lmr)
		}
	}

	if len(declaredModelMap) > 0 && len(pulledModels) == 0 && !s.autoDiscoverModels {
		res.Error = declaredModelsUnavailableError("vLLM", declaredModelMap, res.Models).Error()
	}
	res.PulledModels = pulledModels
	s.state.Store(backend.ID, res)
}

func (s *State) processGeminiBackend(ctx context.Context, backend *runtimetypes.Backend, _ []*runtimetypes.Model) {
	stateInstance := &statetype.BackendRuntimeState{
		ID:           backend.ID,
		Name:         backend.Name,
		Backend:      *backend,
		PulledModels: []statetype.ModelPullStatus{},
	}
	stateInstance.SetAPIKey("")
	apiKey, err := s.loadProviderAPIKey(ctx, backend.Type)
	if err != nil {
		if errors.Is(err, libdb.ErrNotFound) {
			stateInstance.Error = "API key not configured"
		} else {
			stateInstance.Error = fmt.Sprintf("Failed to retrieve API key configuration: %v", err)
		}
		s.state.Store(backend.ID, stateInstance)
		return
	}
	stateInstance.SetAPIKey(apiKey)

	if cachedModels, ok := s.loadObservedModelCache(ctx, backend.ID, apiKey); ok {
		stateInstance.Models = observedModelNames(cachedModels)
		stateInstance.PulledModels = make([]statetype.ModelPullStatus, 0, len(cachedModels))
		for _, model := range cachedModels {
			lmr := s.applyCapabilityOverrides(ctx, backend.Type, pullStatusFromObservedModel(model))
			stateInstance.PulledModels = append(stateInstance.PulledModels, lmr)
		}
		s.state.Store(backend.ID, stateInstance)
		return
	}

	catalog, err := s.newCatalogProvider(backend, apiKey)
	if err != nil {
		stateInstance.Error = err.Error()
		s.state.Store(backend.ID, stateInstance)
		return
	}
	observedModels, err := catalog.ListModels(ctx)
	if err != nil {
		stateInstance.Error = err.Error()
		s.state.Store(backend.ID, stateInstance)
		return
	}

	// Update state
	stateInstance.Models = observedModelNames(observedModels)
	stateInstance.PulledModels = make([]statetype.ModelPullStatus, 0, len(observedModels))
	for _, model := range observedModels {
		lmr := s.applyCapabilityOverrides(ctx, backend.Type, pullStatusFromObservedModel(model))
		stateInstance.PulledModels = append(stateInstance.PulledModels, lmr)
	}
	s.state.Store(backend.ID, stateInstance)

	// Store successful result in cache
	s.storeObservedModelCache(ctx, backend.ID, apiKey, observedModels)
}

// processVertexBackend handles state reconciliation for all vertex-* backend types.
func (s *State) processVertexBackend(ctx context.Context, backend *runtimetypes.Backend, models []*runtimetypes.Model) {
	s.processOptionalCredCloudBackend(ctx, backend, models)
}

// processBedrockBackend handles state reconciliation for AWS Bedrock backends.
func (s *State) processBedrockBackend(ctx context.Context, backend *runtimetypes.Backend, models []*runtimetypes.Model) {
	s.processOptionalCredCloudBackend(ctx, backend, models)
}

// processOptionalCredCloudBackend reconciles a cloud backend whose credentials
// are OPTIONAL: a stored cred blob (Vertex service-account JSON / Bedrock static
// keys) is used when present, and an empty blob is fine — the provider falls
// back to the ambient credential chain (GCP ADC / AWS default chain). This
// differs from processOpenAIBackend, which errors when no API key is stored.
func (s *State) processOptionalCredCloudBackend(ctx context.Context, backend *runtimetypes.Backend, _ []*runtimetypes.Model) {
	stateInstance := &statetype.BackendRuntimeState{
		ID:           backend.ID,
		Name:         backend.Name,
		Backend:      *backend,
		PulledModels: []statetype.ModelPullStatus{},
	}

	// credJSON may be empty (ADC fallback) — that's fine, not an error.
	credJSON, _ := s.loadProviderAPIKey(ctx, backend.Type)
	stateInstance.SetAPIKey(credJSON)

	if cachedModels, ok := s.loadObservedModelCache(ctx, backend.ID, credJSON); ok {
		stateInstance.Models = observedModelNames(cachedModels)
		stateInstance.PulledModels = make([]statetype.ModelPullStatus, 0, len(cachedModels))
		for _, model := range cachedModels {
			lmr := s.applyCapabilityOverrides(ctx, backend.Type, pullStatusFromObservedModel(model))
			stateInstance.PulledModels = append(stateInstance.PulledModels, lmr)
		}
		s.state.Store(backend.ID, stateInstance)
		return
	}

	catalog, err := s.newCatalogProvider(backend, credJSON)
	if err != nil {
		stateInstance.Error = err.Error()
		s.state.Store(backend.ID, stateInstance)
		return
	}
	observedModels, err := catalog.ListModels(ctx)
	if err != nil {
		stateInstance.Error = err.Error()
		s.state.Store(backend.ID, stateInstance)
		return
	}

	stateInstance.Models = observedModelNames(observedModels)
	stateInstance.PulledModels = make([]statetype.ModelPullStatus, 0, len(observedModels))
	for _, model := range observedModels {
		lmr := s.applyCapabilityOverrides(ctx, backend.Type, pullStatusFromObservedModel(model))
		stateInstance.PulledModels = append(stateInstance.PulledModels, lmr)
	}
	s.state.Store(backend.ID, stateInstance)
	s.storeObservedModelCache(ctx, backend.ID, credJSON, observedModels)
}

func (s *State) processOpenAIBackend(ctx context.Context, backend *runtimetypes.Backend, models []*runtimetypes.Model) {
	stateInstance := &statetype.BackendRuntimeState{
		ID:           backend.ID,
		Name:         backend.Name,
		PulledModels: []statetype.ModelPullStatus{},
		Backend:      *backend,
	}

	apiKey, err := s.loadProviderAPIKey(ctx, backend.Type)
	if err != nil {
		if errors.Is(err, libdb.ErrNotFound) {
			stateInstance.Error = "API key not configured"
		} else {
			stateInstance.Error = fmt.Sprintf("Failed to retrieve API key configuration: %v", err)
		}
		s.state.Store(backend.ID, stateInstance)
		return
	}
	stateInstance.SetAPIKey(apiKey)

	// Create lookup map for declared models
	declaredModels := make(map[string]*runtimetypes.Model)
	for _, model := range models {
		name, _ := strings.CutSuffix(model.Model, ":latest")
		declaredModels[name] = model
	}

	observedModels, ok := s.loadObservedModelCache(ctx, backend.ID, apiKey)
	if !ok {
		catalog, err := s.newCatalogProvider(backend, apiKey)
		if err != nil {
			stateInstance.Error = err.Error()
			s.state.Store(backend.ID, stateInstance)
			return
		}
		observedModels, err = catalog.ListModels(ctx)
		if err != nil {
			stateInstance.Error = err.Error()
			s.state.Store(backend.ID, stateInstance)
			return
		}
		s.storeObservedModelCache(ctx, backend.ID, apiKey, observedModels)
	}

	// Update state
	stateInstance.Models = observedModelNames(observedModels)
	pulledModels := make([]statetype.ModelPullStatus, 0, len(observedModels))
	for _, observed := range observedModels {
		if declaredModel, exists := declaredModels[observed.Name]; exists {
			lmr := statetype.ModelPullStatus{
				Name:            declaredModel.ID,
				Model:           declaredModel.Model,
				ModifiedAt:      declaredModel.UpdatedAt,
				ContextLength:   declaredModel.ContextLength,
				MaxOutputTokens: observed.MaxOutputTokens,
				CanChat:         declaredModel.CanChat,
				CanEmbed:        declaredModel.CanEmbed,
				CanPrompt:       declaredModel.CanPrompt,
				CanStream:       declaredModel.CanStream,
			}
			lmr = s.applyCapabilityOverrides(ctx, backend.Type, lmr)
			pulledModels = append(pulledModels, lmr)
			continue
		}
		if s.autoDiscoverModels {
			lmr := s.applyCapabilityOverrides(ctx, backend.Type, pullStatusFromObservedModel(observed))
			pulledModels = append(pulledModels, lmr)
		}
	}
	stateInstance.PulledModels = pulledModels
	if len(declaredModels) > 0 && len(pulledModels) == 0 && !s.autoDiscoverModels {
		stateInstance.Error = declaredModelsUnavailableError("OpenAI", declaredModels, stateInstance.Models).Error()
	}

	s.state.Store(backend.ID, stateInstance)
}

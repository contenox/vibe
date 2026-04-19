// runtimestate implements the core logic for reconciling the declared state
// of LLM backends (from dbInstance) with their actual observed state.
// It keeps runtime observation read-only and is intended to be executed repeatedly
// within background tasks managed externally.
package runtimestate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/contenox/contenox/runtime/internal/modelrepo"
	libbus "github.com/contenox/contenox/libbus"
	libdb "github.com/contenox/contenox/libdbexec"
	"github.com/contenox/contenox/libkvstore"
	"github.com/contenox/contenox/runtime/runtimetypes"
	"github.com/contenox/contenox/runtime/statetype"
)

// ProviderCacheDuration defines how long the state of models from an external
// provider (like OpenAI or Gemini) is cached to avoid frequent API calls.
const ProviderCacheDuration = 1 * time.Hour

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
	if s.withgroups {
		return s.syncBackendsWithgroups(ctx)
	}
	return s.syncBackends(ctx)
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
	switch strings.ToLower(backend.Type) {
	case "ollama":
		s.processOllamaBackend(ctx, backend, declaredModels)
	case "vllm":
		s.processVLLMBackend(ctx, backend, declaredModels)
	case "gemini":
		s.processGeminiBackend(ctx, backend, declaredModels)
	case "openai":
		s.processOpenAIBackend(ctx, backend, declaredModels)
	case "local":
		s.processLocalBackend(ctx, backend, declaredModels)
	case "vertex-google", "vertex-anthropic", "vertex-meta", "vertex-mistralai":
		s.processVertexBackend(ctx, backend, declaredModels)
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

// processLocalBackend handles state reconciliation for a local llama.cpp backend.
// It scans the model directory (stored in backend.BaseURL) for GGUF model subdirectories.
func (s *State) processLocalBackend(ctx context.Context, backend *runtimetypes.Backend, _ []*runtimetypes.Model) {
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
	stateservice := &statetype.BackendRuntimeState{
		ID:      backend.ID,
		Name:    backend.Name,
		Backend: *backend,
		Models:  make([]string, 0, len(observedModels)),
	}
	pulledModels := make([]statetype.ModelPullStatus, 0, len(observedModels))
	for _, observed := range observedModels {
		pulledModels = append(pulledModels, pullStatusFromObservedModel(observed))
	}
	stateservice.PulledModels = pulledModels
	if s.autoDiscoverModels {
		stateservice.Models = observedModelNames(observedModels)
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

			pulledModels = append(pulledModels, statetype.ModelPullStatus{
				Name:          declaredModel.ID,
				Model:         declaredModel.Model,
				ModifiedAt:    declaredModel.UpdatedAt,
				ContextLength: effectiveContextLen,
				CanChat:       declaredModel.CanChat,
				CanEmbed:      declaredModel.CanEmbed,
				CanPrompt:     declaredModel.CanPrompt,
				CanStream:     declaredModel.CanStream,
			})
			continue
		}

		if s.autoDiscoverModels {
			pulledModels = append(pulledModels, pullStatusFromObservedModel(observed))
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
			stateInstance.PulledModels = append(stateInstance.PulledModels, pullStatusFromObservedModel(model))
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
		stateInstance.PulledModels = append(stateInstance.PulledModels, pullStatusFromObservedModel(model))
	}
	s.state.Store(backend.ID, stateInstance)

	// Store successful result in cache
	s.storeObservedModelCache(ctx, backend.ID, apiKey, observedModels)
}

// processVertexBackend handles state reconciliation for all vertex-* backend types.
// Auth uses a stored service account JSON when available; falls back to ADC otherwise.
func (s *State) processVertexBackend(ctx context.Context, backend *runtimetypes.Backend, _ []*runtimetypes.Model) {
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
			stateInstance.PulledModels = append(stateInstance.PulledModels, pullStatusFromObservedModel(model))
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
		stateInstance.PulledModels = append(stateInstance.PulledModels, pullStatusFromObservedModel(model))
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
			pulledModels = append(pulledModels, statetype.ModelPullStatus{
				Name:          declaredModel.ID,
				Model:         declaredModel.Model,
				ModifiedAt:    declaredModel.UpdatedAt,
				ContextLength: declaredModel.ContextLength,
				CanChat:       declaredModel.CanChat,
				CanEmbed:      declaredModel.CanEmbed,
				CanPrompt:     declaredModel.CanPrompt,
				CanStream:     declaredModel.CanStream,
			})
			continue
		}
		if s.autoDiscoverModels {
			pulledModels = append(pulledModels, pullStatusFromObservedModel(observed))
		}
	}
	stateInstance.PulledModels = pulledModels
	if len(declaredModels) > 0 && len(pulledModels) == 0 && !s.autoDiscoverModels {
		stateInstance.Error = declaredModelsUnavailableError("OpenAI", declaredModels, stateInstance.Models).Error()
	}

	s.state.Store(backend.ID, stateInstance)
}

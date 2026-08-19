// Package runtimestate reconciles the declared state of LLM backends against
// their observed state, read-only, intended to run repeatedly from a background
// task. State exposes the result through ProviderFromRuntimeState.
package runtimestate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/contenox/contenox/internal/models/modelrepo"
	"github.com/contenox/contenox/internal/store/runtimetypes"
	libbus "github.com/contenox/contenox/libbus"
	libdb "github.com/contenox/contenox/libdbexec"
	"github.com/contenox/contenox/libkvstore"
)

// ProviderCacheDuration defines how long the state of models from an external
// provider (like OpenAI or Gemini) is cached to avoid frequent API calls.
const ProviderCacheDuration = 1 * time.Hour

// ReconcileDebounceInterval bounds how often a read-triggered reconcile
// (ReconcileIfStale) re-scans backends, so a burst of UI polls cannot
// stampede a full cycle. A package var so tests can shorten it.
var ReconcileDebounceInterval = 15 * time.Second

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

// New creates a State manager backed by dbInstance and psInstance; options
// enable optional features such as group-based reconciliation.
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
	for _, option := range options {
		option(s)
	}
	return s, nil
}

// RunBackendCycle performs one reconciliation check for all configured LLM
// backends and refreshes the runtime snapshot. Scheduling, lifecycle, and retry
// policy are the caller's responsibility.
func (s *State) RunBackendCycle(ctx context.Context) error {
	var err error
	if s.withgroups {
		err = s.syncBackendsWithgroups(ctx)
	} else {
		err = s.syncBackends(ctx)
	}
	// Feeds the debounce clock (even on error) so a read-triggered cycle right
	// after is suppressed rather than polling a failing backend in a hot loop.
	s.markReconciled(time.Now())
	return err
}

// ReconcileIfStale runs one RunBackendCycle when the last reconcile is older
// than ReconcileDebounceInterval, otherwise it is a no-op. Read paths call it to
// self-heal when a backend comes up after the last reconcile.
func (s *State) ReconcileIfStale(ctx context.Context) error {
	if !s.claimReconcile(time.Now()) {
		return nil
	}
	return s.RunBackendCycle(ctx)
}

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
func (s *State) Get(ctx context.Context) map[string]BackendRuntimeState {
	state := map[string]BackendRuntimeState{}
	s.state.Range(func(key, value any) bool {
		backend, ok := value.(*BackendRuntimeState)
		if !ok {
			return true
		}
		var backendCopy BackendRuntimeState
		raw, err := json.Marshal(backend)
		if err != nil {
		}
		err = json.Unmarshal(raw, &backendCopy)
		if err != nil {
		}
		backendCopy.SetAPIKey(backend.GetAPIKey())
		state[backend.ID] = backendCopy
		return true
	})
	return state
}

func (s *State) cleanupStaleBackends(currentIDs map[string]struct{}) error {
	var err error
	s.state.Range(func(key, value any) bool {
		id, ok := key.(string)
		if !ok {
			err = fmt.Errorf("BUG: invalid key type: %T %v", key, key)
			return true
		}
		if _, exists := currentIDs[id]; !exists {
			s.state.Delete(id)
		}
		return true
	})
	return err
}

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

	for backendID, backendObj := range allBackendObjects {
		modelsForThisBackend := make([]*runtimetypes.Model, 0, len(backendToAggregatedModels[backendID]))
		for _, model := range backendToAggregatedModels[backendID] {
			modelsForThisBackend = append(modelsForThisBackend, model)
		}
		s.processBackend(ctx, backendObj, modelsForThisBackend)
	}

	return s.cleanupStaleBackends(activeBackendIDs)
}

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

func (s *State) processBackends(ctx context.Context, backends []*runtimetypes.Backend, models []*runtimetypes.Model, currentIDs map[string]struct{}) {
	for _, backend := range backends {
		currentIDs[backend.ID] = struct{}{}
		s.processBackend(ctx, backend, models)
	}
}

func (s *State) processBackend(ctx context.Context, backend *runtimetypes.Backend, declaredModels []*runtimetypes.Model) {
	switch modelrepo.CanonicalBackendType(backend.Type) {
	case "ollama":
		s.processOllamaBackend(ctx, backend, declaredModels)
	case "vllm":
		s.processVLLMBackend(ctx, backend, declaredModels)
	case "gemini":
		s.processGeminiBackend(ctx, backend, declaredModels)
	case "openai", "anthropic":
		s.processOpenAIBackend(ctx, backend, declaredModels)
	case "vertex-google":
		s.processVertexBackend(ctx, backend, declaredModels)
	case "bedrock":
		s.processBedrockBackend(ctx, backend, declaredModels)
	case modelrepo.ScriptedTestBackendType:
		s.processScriptedTestBackend(ctx, backend)
	default:
		brokenService := &BackendRuntimeState{
			ID:      backend.ID,
			Name:    backend.Name,
			Models:  []string{},
			Backend: *backend,
			Error:   "Unsupported backend type: " + backend.Type,
		}
		s.state.Store(backend.ID, brokenService)
	}
}

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

	stateservice := &BackendRuntimeState{
		ID:      backend.ID,
		Name:    backend.Name,
		Backend: *backend,
		Models:  make([]string, 0, len(observedModels)),
	}
	stateservice.SetAPIKey(apiKey)

	pulledModels := make([]ModelPullStatus, 0, len(observedModels))
	for _, observed := range observedModels {
		lmr := pullStatusFromObservedModel(observed)

		// Write a discovered context_length back so later cycles skip re-learning it.
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

func (s *State) processVLLMBackend(ctx context.Context, backend *runtimetypes.Backend, models []*runtimetypes.Model) {
	declaredModelMap := make(map[string]*runtimetypes.Model)
	for _, m := range models {
		declaredModelMap[m.Model] = m
	}

	// The bearer token must reach both the catalog and the runtime state.
	apiKey := ""
	if key, err := s.loadProviderAPIKey(ctx, backend.Type); err == nil {
		apiKey = key
	}

	catalog, err := s.newCatalogProvider(backend, apiKey)
	if err != nil {
		storeBackendError(s, backend, apiKey, err, nil)
		return
	}

	observedModels, err := catalog.ListModels(ctx)
	if err != nil {
		storeBackendError(s, backend, apiKey, err, nil)
		return
	}
	if len(observedModels) == 0 {
		storeBackendError(s, backend, apiKey, fmt.Errorf("No models found in response"), nil)
		return
	}

	res := &BackendRuntimeState{
		ID:      backend.ID,
		Name:    backend.Name,
		Models:  observedModelNames(observedModels),
		Backend: *backend,
	}
	res.SetAPIKey(apiKey)

	pulledModels := make([]ModelPullStatus, 0, len(observedModels))
	for _, observed := range observedModels {
		if declaredModel, exists := declaredModelMap[observed.Name]; exists {
			effectiveContextLen := declaredModel.ContextLength
			if effectiveContextLen == 0 && observed.ContextLength > 0 {
				effectiveContextLen = observed.ContextLength
				declCopy := *declaredModel
				declCopy.ContextLength = observed.ContextLength
				_ = runtimetypes.New(s.dbInstance.WithoutTransaction()).UpdateModel(ctx, &declCopy)
			}

			// Observed capabilities are the base; declared trues merge in additively.
			lmr := mergeDeclaredOverObserved(declaredModel, observed)
			lmr.ContextLength = effectiveContextLen
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
	stateInstance := &BackendRuntimeState{
		ID:           backend.ID,
		Name:         backend.Name,
		Backend:      *backend,
		PulledModels: []ModelPullStatus{},
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
		stateInstance.PulledModels = make([]ModelPullStatus, 0, len(cachedModels))
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

	stateInstance.Models = observedModelNames(observedModels)
	stateInstance.PulledModels = make([]ModelPullStatus, 0, len(observedModels))
	for _, model := range observedModels {
		lmr := s.applyCapabilityOverrides(ctx, backend.Type, pullStatusFromObservedModel(model))
		stateInstance.PulledModels = append(stateInstance.PulledModels, lmr)
	}
	s.state.Store(backend.ID, stateInstance)

	s.storeObservedModelCache(ctx, backend.ID, apiKey, observedModels)
}

func (s *State) processVertexBackend(ctx context.Context, backend *runtimetypes.Backend, models []*runtimetypes.Model) {
	s.processOptionalCredCloudBackend(ctx, backend, models)
}

func (s *State) processBedrockBackend(ctx context.Context, backend *runtimetypes.Backend, models []*runtimetypes.Model) {
	s.processOptionalCredCloudBackend(ctx, backend, models)
}

func (s *State) processOptionalCredCloudBackend(ctx context.Context, backend *runtimetypes.Backend, _ []*runtimetypes.Model) {
	stateInstance := &BackendRuntimeState{
		ID:           backend.ID,
		Name:         backend.Name,
		Backend:      *backend,
		PulledModels: []ModelPullStatus{},
	}

	// credJSON may be empty (ADC fallback) — that's fine, not an error.
	credJSON, _ := s.loadProviderAPIKey(ctx, backend.Type)
	stateInstance.SetAPIKey(credJSON)

	if cachedModels, ok := s.loadObservedModelCache(ctx, backend.ID, credJSON); ok {
		stateInstance.Models = observedModelNames(cachedModels)
		stateInstance.PulledModels = make([]ModelPullStatus, 0, len(cachedModels))
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
	stateInstance.PulledModels = make([]ModelPullStatus, 0, len(observedModels))
	for _, model := range observedModels {
		lmr := s.applyCapabilityOverrides(ctx, backend.Type, pullStatusFromObservedModel(model))
		stateInstance.PulledModels = append(stateInstance.PulledModels, lmr)
	}
	s.state.Store(backend.ID, stateInstance)
	s.storeObservedModelCache(ctx, backend.ID, credJSON, observedModels)
}

// processScriptedTestBackend re-reads the script every cycle and never caches its model list, so editing the script is enough to change what the backend offers.
func (s *State) processScriptedTestBackend(ctx context.Context, backend *runtimetypes.Backend) {
	stateInstance := &BackendRuntimeState{
		ID:           backend.ID,
		Name:         backend.Name,
		Backend:      *backend,
		PulledModels: []ModelPullStatus{},
	}
	stateInstance.SetAPIKey("")

	catalog, err := s.newCatalogProvider(backend, "")
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
	for _, model := range observedModels {
		stateInstance.PulledModels = append(stateInstance.PulledModels, s.applyCapabilityOverrides(ctx, backend.Type, pullStatusFromObservedModel(model)))
	}
	s.state.Store(backend.ID, stateInstance)
}

func (s *State) processOpenAIBackend(ctx context.Context, backend *runtimetypes.Backend, models []*runtimetypes.Model) {
	stateInstance := &BackendRuntimeState{
		ID:           backend.ID,
		Name:         backend.Name,
		PulledModels: []ModelPullStatus{},
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

	stateInstance.Models = observedModelNames(observedModels)
	pulledModels := make([]ModelPullStatus, 0, len(observedModels))
	for _, observed := range observedModels {
		if declaredModel, exists := declaredModels[observed.Name]; exists {
			// Observed capabilities are the base; declared trues merge in additively.
			lmr := mergeDeclaredOverObserved(declaredModel, observed)
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

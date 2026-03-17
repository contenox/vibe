// runtimestate implements the core logic for reconciling the declared state
// of LLM backends (from dbInstance) with their actual observed state.
// It provides the functionality for synchronizing models and processing downloads,
// intended to be executed repeatedly within background tasks managed externally.
package runtimestate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	libbus "github.com/contenox/contenox/libbus"
	libdb "github.com/contenox/contenox/libdbexec"
	"github.com/contenox/contenox/libkvstore"
	"github.com/contenox/contenox/runtimetypes"
	"github.com/contenox/contenox/statetype"
	"github.com/ollama/ollama/api"
)

// ProviderCacheDuration defines how long the state of models from an external
// provider (like OpenAI or Gemini) is cached to avoid frequent API calls.
const ProviderCacheDuration = 1 * time.Hour

// providerCacheEntry holds the data and metadata for a cached provider state.
// APIKey is stored so we can detect key rotation and invalidate the cache.
type providerCacheEntry struct {
	Models []statetype.ModelPullStatus `json:"models"`
	APIKey string                      `json:"api_key"`
}

// State manages the overall runtime status of multiple LLM backends.
// It orchestrates the synchronization between the desired configuration
// and the actual state of the backends, including providing the mechanism
// for model downloads via the dwqueue component.
type State struct {
	dbInstance           libdb.DBManager
	state                sync.Map
	psInstance           libbus.Messenger
	dwQueue              dwqueue
	withgroups           bool
	skipDeleteUndeclared bool // when true, do not delete Ollama models that are not declared (for pre-pulled models)
	autoDiscoverModels   bool // when true, expose all live backend models without requiring declaration
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

// WithSkipDeleteUndeclaredModels prevents deletion of backend models that are not in the declared set.
// Use when models are pre-pulled (e.g. ollama pull phi3:3.8b) and you do not want sync to remove them.
func WithSkipDeleteUndeclaredModels() Option {
	return func(s *State) {
		s.skipDeleteUndeclared = true
	}
}

// WithAutoDiscoverModels exposes all models returned by live backends without requiring manual
// declaration via 'model add'. Capability inference is name-based for providers (e.g. OpenAI)
// whose APIs do not return capability metadata. The fleet-management declare-to-pull
// behaviour for Ollama is preserved; only the PulledModels exposure is widened.
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
		dwQueue:    dwqueue{dbInstance: dbInstance},
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
// (by communicating with the backends) and schedules necessary actions,
// such as queuing model downloads or removals, to align them.
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

// RunDownloadCycle processes a single pending model download operation, if one exists.
// It retrieves the next download task, executes the download while providing
// progress updates, and handles potential cancellation requests.
// If no download tasks are queued, it returns nil immediately.
// This method should be called periodically in a background process to
// drain the download queue.
// DESIGN NOTE: this method performs one unit of work
// and returns. The caller is responsible for the execution loop, allowing
// flexible integration with task management strategies.
//
// This method should be called periodically by an external process to
// drain the download queue.
func (s *State) RunDownloadCycle(ctx context.Context) error {
	item, err := s.dwQueue.pop(ctx)
	if err != nil {
		if errors.Is(err, libdb.ErrNotFound) {
			return nil
		}
		return err
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel() // clean up the context when done

	done := make(chan struct{})

	ch := make(chan []byte, 16)
	sub, err := s.psInstance.Stream(ctx, "queue_cancel", ch)
	if err != nil {
		// log.Println("Error subscribing to queue_cancel:", err)
		return nil
	}
	go func() {
		defer func() {
			sub.Unsubscribe()
			close(done)
		}()
		for {
			select {
			case data, ok := <-ch:
				if !ok {
					return
				}
				var queueItem runtimetypes.Job
				if err := json.Unmarshal(data, &queueItem); err != nil {
					// log.Println("Error unmarshalling cancel message:", err)
					continue
				}
				// Check if the cancellation request matches the current download task.
				// Rationale: Matching logic based on URL to target a specific backend
				// or Model ID to purge a model from all backends, if it is currently downloading.
				if queueItem.ID == item.URL || queueItem.ID == item.Model {
					cancel()
				}
			case <-ctx.Done():
				return
			}
		}
	}()

	// log.Printf("Processing download job: %+v", item)
	err = s.dwQueue.downloadModel(ctx, *item, func(status runtimetypes.Status) error {
		// //log.Printf("Download progress for model %s: %+v", item.Model, status)
		message, _ := json.Marshal(status)
		return s.psInstance.Publish(ctx, "model_download", message)
	})
	if err != nil {
		return fmt.Errorf("failed downloading model %s: %w", item.Model, err)
	}

	cancel()
	<-done

	return nil
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

// processOllamaBackend handles the state reconciliation for a single Ollama backend.
// It connects to the Ollama API, compares the set of declared models for this backend
// with the models actually present on the Ollama instance, and takes corrective actions:
// - Queues downloads for declared models that are missing.
// - Initiates deletion for models present on the instance but not declared in the config.
// Finally, it updates the internal state map with the latest observed list of pulled models
// and any communication errors encountered.
func (s *State) processOllamaBackend(ctx context.Context, backend *runtimetypes.Backend, declaredOllamaModels []*runtimetypes.Model) {
	// log.Printf("Processing Ollama backend for ID %s with declared models: %+v", backend.ID, declaredOllamaModels)

	models := []string{}
	declaredModelMap := make(map[string]runtimetypes.Model)
	for _, model := range declaredOllamaModels {
		declaredModelMap[model.Model] = *model
		models = append(models, model.Model)
	}
	// log.Printf("Extracted model names for backend %s: %v", backend.ID, models)

	backendURL, err := url.Parse(backend.BaseURL)
	if err != nil {
		// log.Printf("Error parsing URL for backend %s: %v", backend.ID, err)
		stateservice := &statetype.BackendRuntimeState{
			ID:           backend.ID,
			Name:         backend.Name,
			Models:       models,
			PulledModels: []statetype.ModelPullStatus{},
			Backend:      *backend,
			Error:        "Invalid URL: " + err.Error(),
		}
		s.state.Store(backend.ID, stateservice)
		return
	}
	// log.Printf("Parsed URL for backend %s: %s", backend.ID, backendURL.String())

	client := api.NewClient(backendURL, http.DefaultClient)
	existingModels, err := client.List(ctx)
	if err != nil {
		// log.Printf("Error listing models for backend %s: %v", backend.ID, err)
		stateservice := &statetype.BackendRuntimeState{
			ID:           backend.ID,
			Name:         backend.Name,
			Models:       models,
			PulledModels: []statetype.ModelPullStatus{},
			Backend:      *backend,
			Error:        err.Error(),
		}
		s.state.Store(backend.ID, stateservice)
		return
	}
	// log.Printf("Existing models from backend %s: %+v", backend.ID, existingModels.Models)

	declaredModelSet := make(map[string]struct{})
	for _, declaredModel := range declaredOllamaModels {
		declaredModelSet[declaredModel.Model] = struct{}{}
	}
	// log.Printf("Declared model set for backend %s: %v", backend.ID, declaredModelSet)

	existingModelSet := make(map[string]struct{})
	for _, existingModel := range existingModels.Models {
		existingModelSet[existingModel.Model] = struct{}{}
	}
	// log.Printf("Existing model set for backend %s: %v", backend.ID, existingModelSet)

	// For each declared model missing from the backend, add a download job.
	for declaredModel := range declaredModelSet {
		if _, ok := existingModelSet[declaredModel]; !ok {
			// log.Printf("Model %s is declared but missing in backend %s. Adding to download queue.", declaredModel, backend.ID)
			// RATIONALE: Using the backend URL as the Job ID in the queue prevents
			// queueing multiple downloads for the same backend simultaneously,
			// acting as a simple lock at the queue level.
			// Download flow:
			// 1. The sync cycle re-evaluates the full desired vs. actual state
			//     periodically. It will re-detect *all* currently missing models on each run.
			// 2. Therefore, the queue doesn't need to store a "TODO" list of all
			//     pending downloads for a backend. A single job per backend URL acts as
			//     a sufficient signal that *a* download action is required.
			// 3. The specific model placed in this job's payload reflects one missing model
			//     identified during the *most recent* sync cycle run.
			// 4. When this model is downloaded, the *next* sync cycle will identify the
			//     *next* missing model (if any) and trigger the queue again, eventually
			//     leading to all models being downloaded over successive cycles.
			// 5. If the backeend dies while downloading this mechanism will ensure that
			//     the downloadjob will be readded to the queue.
			err := s.dwQueue.add(ctx, *backendURL, declaredModel)
			if err != nil {
				// log.Printf("Error adding model %s to download queue: %v", declaredModel, err)
			}
		}
	}

	// For each model in the backend that is not declared, trigger deletion (unless skipDeleteUndeclared).
	// NOTE: We have to delete otherwise we have keep track of not desired model in each backend to
	// ensure some backend-nodes don't just run out of space.
	if !s.skipDeleteUndeclared {
		for existingModel := range existingModelSet {
			if _, ok := declaredModelSet[existingModel]; !ok {
				// log.Printf("Model %s exists in backend %s but is not declared. Triggering deletion.", existingModel, backend.ID)
				err := client.Delete(ctx, &api.DeleteRequest{
					Model: existingModel,
				})
				if err != nil {
					// log.Printf("Error deleting model %s for backend %s: %v", existingModel, backend.ID, err)
				} else {
					// log.Printf("Successfully deleted model %s for backend %s", existingModel, backend.ID)
				}
			}
		}
	}

	modelResp, err := client.List(ctx)
	if err != nil {
		// log.Printf("Error listing running models for backend %s after deletion: %v", backend.ID, err)
		stateservice := &statetype.BackendRuntimeState{
			ID:           backend.ID,
			Name:         backend.Name,
			Models:       models,
			PulledModels: []statetype.ModelPullStatus{},
			Backend:      *backend,
			Error:        err.Error(),
		}
		s.state.Store(backend.ID, stateservice)
		return
	}
	// log.Printf("Updated model list for backend %s: %+v", backend.ID, modelResp.Models)

	stateservice := &statetype.BackendRuntimeState{
		ID:      backend.ID,
		Name:    backend.Name,
		Backend: *backend,
		Models:  make([]string, 0, len(modelResp.Models)),
	}

	// Create proper model entries with capabilities
	pulledModels := make([]statetype.ModelPullStatus, 0, len(modelResp.Models))
	for _, model := range modelResp.Models {
		lmr := statetype.ConvertOllamaModelResponse(&model)

		// Auto-fetch capabilities and context_length from Ollama /api/show.
		// This mirrors fetchGeminiModelInfo: we trust the provider's own metadata
		// rather than requiring manual declaration.
		if showResp, err := client.Show(ctx, &api.ShowRequest{Model: model.Model}); err == nil {
			statetype.EnrichFromOllamaShow(lmr, showResp)

			// If the declared model has no context_length yet (auto-detect placeholder),
			// write the discovered value back to the DB so subsequent cycles skip Show.
			if decl, exists := declaredModelMap[lmr.Name]; exists && decl.ContextLength == 0 && lmr.ContextLength > 0 {
				declCopy := decl
				declCopy.ContextLength = lmr.ContextLength
				declCopy.CanChat = lmr.CanChat
				declCopy.CanEmbed = lmr.CanEmbed
				declCopy.CanPrompt = lmr.CanPrompt
				declCopy.CanStream = lmr.CanStream
				_ = runtimetypes.New(s.dbInstance.WithoutTransaction()).UpdateModel(ctx, &declCopy)
			}
		}

		// Declared caps act as explicit overrides (admin intent wins over auto-detected values).
		if declaredModel, exists := declaredModelMap[lmr.Name]; exists {
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

		pulledModels = append(pulledModels, *lmr)
	}

	stateservice.PulledModels = pulledModels
	if s.autoDiscoverModels {
		// Expose all currently-pulled Ollama models by name, not just declared ones.
		allPulledNames := make([]string, 0, len(pulledModels))
		for _, pm := range pulledModels {
			allPulledNames = append(allPulledNames, pm.Model)
		}
		stateservice.Models = allPulledNames
	} else {
		stateservice.Models = models
	}
	s.state.Store(backend.ID, stateservice)
	// log.Printf("Stored updated state for backend %s", backend.ID)

}

// processVLLMBackend handles the state reconciliation for a single vLLM backend.
// Since vLLM instances typically serve a single model, we verify that the running model
// matches one of the models assigned to the backend through its groups.
func (s *State) processVLLMBackend(ctx context.Context, backend *runtimetypes.Backend, models []*runtimetypes.Model) {
	// Create HTTP client with timeout
	client := &http.Client{
		Timeout: 5 * time.Second,
	}
	wantedModels := []string{}
	declaredModelMap := make(map[string]*runtimetypes.Model)
	for _, m := range models {
		wantedModels = append(wantedModels, m.Model)
		declaredModelMap[m.Model] = m
	}
	// Build models endpoint URL
	modelsURL := strings.TrimSuffix(backend.BaseURL, "/") + "/v1/models"
	req, err := http.NewRequestWithContext(ctx, "GET", modelsURL, nil)
	if err != nil {
		s.state.Store(backend.ID, &statetype.BackendRuntimeState{
			ID:           backend.ID,
			Name:         backend.Name,
			Models:       []string{},
			PulledModels: []statetype.ModelPullStatus{},
			Backend:      *backend,
			Error:        fmt.Sprintf("Failed to create request: %v", err),
		})
		return
	}

	resp, err := client.Do(req)
	if err != nil {
		s.state.Store(backend.ID, &statetype.BackendRuntimeState{
			ID:           backend.ID,
			Name:         backend.Name,
			Models:       []string{},
			PulledModels: []statetype.ModelPullStatus{},
			Backend:      *backend,
			Error:        fmt.Sprintf("HTTP request failed: %v", err),
		})
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		bodyBytes, readErr := io.ReadAll(resp.Body)
		bodyStr := string(bodyBytes)
		if readErr != nil {
			bodyStr = fmt.Sprintf("<failed to read body: %v>", readErr)
		}
		s.state.Store(backend.ID, &statetype.BackendRuntimeState{
			ID:           backend.ID,
			Name:         backend.Name,
			Models:       []string{},
			PulledModels: []statetype.ModelPullStatus{},
			Backend:      *backend,
			Error:        fmt.Sprintf("Unexpected status: %d %s", resp.StatusCode, bodyStr),
		})
		return
	}

	var modelResp struct {
		Object string `json:"object"`
		Data   []struct {
			ID          string `json:"id"`
			MaxModelLen int    `json:"max_model_len"`
		} `json:"data"`
	}

	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		s.state.Store(backend.ID, &statetype.BackendRuntimeState{
			ID:           backend.ID,
			Name:         backend.Name,
			Models:       []string{},
			PulledModels: []statetype.ModelPullStatus{},
			Backend:      *backend,
			Error:        fmt.Sprintf("Failed to read response body: %v", err),
		})
		return
	}
	if err := json.Unmarshal(bodyBytes, &modelResp); err != nil {
		s.state.Store(backend.ID, &statetype.BackendRuntimeState{
			ID:           backend.ID,
			Name:         backend.Name,
			Models:       []string{},
			PulledModels: []statetype.ModelPullStatus{},
			Backend:      *backend,
			Error:        fmt.Sprintf("Failed to decode response: %v | Raw response: %s", err, string(bodyBytes)),
		})
		return
	}

	if len(modelResp.Data) == 0 {
		s.state.Store(backend.ID, &statetype.BackendRuntimeState{
			ID:           backend.ID,
			Name:         backend.Name,
			Models:       []string{},
			PulledModels: []statetype.ModelPullStatus{},
			Backend:      *backend,
			Error:        "No models found in response",
		})
		return
	}

	servedModel := modelResp.Data[0].ID
	// Create mock PulledModels for state reporting
	res := &statetype.BackendRuntimeState{
		ID:      backend.ID,
		Name:    backend.Name,
		Models:  []string{servedModel},
		Backend: *backend,
	}
	pulledModels := []statetype.ModelPullStatus{
		{
			Model: servedModel,
		},
	}
	found := false
	for _, m := range models {
		if m.Model == servedModel {
			found = true

			// Auto-detect context length from vLLM when the admin hasn't set one.
			// Mirrors the Ollama EnrichFromOllamaShow pattern: provider metadata wins
			// only when ContextLength == 0 (the "let the API be authoritative" sentinel).
			vllmContextLen := modelResp.Data[0].MaxModelLen
			effectiveContextLen := m.ContextLength
			if effectiveContextLen == 0 && vllmContextLen > 0 {
				effectiveContextLen = vllmContextLen
				// Persist the discovered value so subsequent cycles skip the override.
				declCopy := *m
				declCopy.ContextLength = vllmContextLen
				_ = runtimetypes.New(s.dbInstance.WithoutTransaction()).UpdateModel(ctx, &declCopy)
			}

			pulledModels[0] = statetype.ModelPullStatus{
				Name:          m.ID,
				Model:         m.Model,
				ModifiedAt:    m.UpdatedAt,
				ContextLength: effectiveContextLen,
				CanChat:       m.CanChat,
				CanEmbed:      m.CanEmbed,
				CanPrompt:     m.CanPrompt,
				CanStream:     m.CanStream,
			}
		}
	}
	if !found {
		if s.autoDiscoverModels {
			pulledModels[0] = statetype.ModelPullStatus{
				Name:          servedModel,
				Model:         servedModel,
				ModifiedAt:    time.Now().UTC(),
				ContextLength: modelResp.Data[0].MaxModelLen,
				CanChat:       true,
				CanEmbed:      false,
				CanPrompt:     true,
				CanStream:     true,
			}
		} else {
			res.Error = fmt.Sprintf("backend has model %s, yet it's not declared in the configuration", servedModel)
		}
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
	// Retrieve API key configuration
	cfg := ProviderConfig{}
	storeInstance := runtimetypes.New(s.dbInstance.WithoutTransaction())
	if err := storeInstance.GetKV(ctx, GeminiKey, &cfg); err != nil {
		if errors.Is(err, libdb.ErrNotFound) {
			stateInstance.Error = "API key not configured"
		} else {
			stateInstance.Error = fmt.Sprintf("Failed to retrieve API key configuration: %v", err)
		}
		s.state.Store(backend.ID, stateInstance)
		return
	}

	// Check cache: prefer kvStore (persistent + TTL-aware); fall back to in-memory sync.Map.
	if s.kvStore != nil {
		if exec, err := s.kvStore.Executor(ctx); err == nil {
			if raw, err := exec.Get(ctx, "prov:"+backend.ID); err == nil {
				var entry providerCacheEntry
				if json.Unmarshal(raw, &entry) == nil && entry.APIKey == cfg.APIKey && len(entry.Models) > 0 {
					modelNames := make([]string, 0, len(entry.Models))
					for _, m := range entry.Models {
						modelNames = append(modelNames, m.Model)
					}
					stateInstance.Models = modelNames
					stateInstance.PulledModels = entry.Models
					stateInstance.SetAPIKey(entry.APIKey)
					s.state.Store(backend.ID, stateInstance)
					return
				}
			}
		}
	} else if cached, ok := s.providerCache.Load(backend.ID); ok {
		if entry, ok := cached.(providerCacheEntry); ok {
			if entry.APIKey == cfg.APIKey && len(entry.Models) > 0 {
				modelNames := make([]string, 0, len(entry.Models))
				for _, m := range entry.Models {
					modelNames = append(modelNames, m.Model)
				}
				stateInstance.Models = modelNames
				stateInstance.PulledModels = entry.Models
				stateInstance.SetAPIKey(entry.APIKey)
				s.state.Store(backend.ID, stateInstance)
				return
			}
		}
	}

	// Prepare HTTP request
	client := &http.Client{Timeout: 10 * time.Second}
	reqURL := fmt.Sprintf("%s/v1beta/models",
		backend.BaseURL,
	)
	req, err := http.NewRequestWithContext(ctx, "GET", reqURL, nil)
	if err != nil {
		stateInstance.Error = fmt.Sprintf("Request creation failed: %v", err)
		s.state.Store(backend.ID, stateInstance)
		return
	}

	req.Header.Set("X-Goog-Api-Key", cfg.APIKey)
	// Execute request
	resp, err := client.Do(req)
	if err != nil {
		stateInstance.Error = fmt.Sprintf("HTTP request failed: %v", err)
		s.state.Store(backend.ID, stateInstance)
		return
	}
	defer resp.Body.Close()

	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		stateInstance.Error = fmt.Sprintf("Failed to read response body: %v", err)
		s.state.Store(backend.ID, stateInstance)
		return
	}

	// Handle non-200 responses
	if resp.StatusCode != http.StatusOK {
		stateInstance.Error = fmt.Sprintf("API returned %d: %s", resp.StatusCode, string(bodyBytes))
		s.state.Store(backend.ID, stateInstance)
		return
	}

	// Parse response
	var geminiResponse struct {
		Models []struct {
			Name string `json:"name"`
		} `json:"models"`
	}
	if err := json.Unmarshal(bodyBytes, &geminiResponse); err != nil {
		stateInstance.Error = fmt.Sprintf("Response parsing failed: %v | Raw response: %s", err, string(bodyBytes))
		s.state.Store(backend.ID, stateInstance)
		return
	}

	modelNames := make([]string, 0, len(geminiResponse.Models))
	pulledModels := make([]statetype.ModelPullStatus, 0, len(geminiResponse.Models))
	for _, m := range geminiResponse.Models {
		resp, err := fetchGeminiModelInfo(ctx, backend.BaseURL, m.Name, cfg.APIKey, http.DefaultClient)
		if err != nil {
			stateInstance.Error = fmt.Sprintf("Failed to fetch model info: %v", err)
			s.state.Store(backend.ID, stateInstance)
			continue
		}
		modelNames = append(modelNames, m.Name)
		pulledModels = append(pulledModels, *resp)
	}

	// Update state
	stateInstance.Models = modelNames
	stateInstance.PulledModels = pulledModels
	stateInstance.SetAPIKey(cfg.APIKey)
	s.state.Store(backend.ID, stateInstance)

	// Store successful result in cache
	newEntry := providerCacheEntry{Models: pulledModels, APIKey: cfg.APIKey}
	if s.kvStore != nil {
		if exec, err := s.kvStore.Executor(ctx); err == nil {
			if data, err := json.Marshal(newEntry); err == nil {
				_ = exec.SetWithTTL(ctx, "prov:"+backend.ID, data, ProviderCacheDuration)
			}
		}
	} else {
		s.providerCache.Store(backend.ID, newEntry)
	}
}

func (s *State) processOpenAIBackend(ctx context.Context, backend *runtimetypes.Backend, models []*runtimetypes.Model) {
	stateInstance := &statetype.BackendRuntimeState{
		ID:           backend.ID,
		Name:         backend.Name,
		PulledModels: []statetype.ModelPullStatus{},
		Backend:      *backend,
	}

	// Retrieve API key configuration
	cfg := ProviderConfig{}
	storeInstance := runtimetypes.New(s.dbInstance.WithoutTransaction())
	if err := storeInstance.GetKV(ctx, OpenaiKey, &cfg); err != nil {
		if errors.Is(err, libdb.ErrNotFound) {
			stateInstance.Error = "API key not configured"
		} else {
			stateInstance.Error = fmt.Sprintf("Failed to retrieve API key configuration: %v", err)
		}
		s.state.Store(backend.ID, stateInstance)
		return
	}

	// Create lookup map for declared models
	declaredModels := make(map[string]*runtimetypes.Model)
	for _, model := range models {
		model.Model, _ = strings.CutSuffix(model.Model, ":latest")
		declaredModels[model.Model] = model
	}

	// Check cache: prefer kvStore (persistent + TTL-aware); fall back to in-memory sync.Map.
	if s.kvStore != nil {
		if exec, err := s.kvStore.Executor(ctx); err == nil {
			if raw, err := exec.Get(ctx, "prov:"+backend.ID); err == nil {
				var entry providerCacheEntry
				if json.Unmarshal(raw, &entry) == nil && entry.APIKey == cfg.APIKey && len(entry.Models) > 0 {
					allModelNames := make([]string, 0, len(entry.Models))
					for _, m := range entry.Models {
						allModelNames = append(allModelNames, m.Model)
					}
					pulledModels := make([]statetype.ModelPullStatus, 0, len(entry.Models))
					for _, m := range entry.Models {
						if declaredModel, exists := declaredModels[m.Name]; exists {
							enhancedModel := m
							enhancedModel.Name = declaredModel.ID
							enhancedModel.Model = declaredModel.Model
							enhancedModel.ContextLength = declaredModel.ContextLength
							enhancedModel.CanChat = declaredModel.CanChat
							enhancedModel.CanEmbed = declaredModel.CanEmbed
							enhancedModel.CanPrompt = declaredModel.CanPrompt
							enhancedModel.CanStream = declaredModel.CanStream
							pulledModels = append(pulledModels, enhancedModel)
						} else if s.autoDiscoverModels {
							pulledModels = append(pulledModels, inferOpenAICapabilities(m.Model))
						}
					}
					stateInstance.Models = allModelNames
					stateInstance.PulledModels = pulledModels
					stateInstance.SetAPIKey(entry.APIKey)
					if len(declaredModels) > 0 && len(pulledModels) == 0 && !s.autoDiscoverModels {
						declaredMap := []string{}
						for k, n := range declaredModels {
							p := "model-data==nil"
							if n != nil {
								p = n.ID + " " + n.Model
							}
							declaredMap = append(declaredMap, k+":"+p)
						}
						stateInstance.Error = fmt.Sprintf("None of the declared models are available in the OpenAI API: declared models: %v \navailable models %s", strings.Join(declaredMap, ", "), allModelNames)
					}
					s.state.Store(backend.ID, stateInstance)
					return
				}
			}
		}
	} else if cached, ok := s.providerCache.Load(backend.ID); ok {
		if entry, ok := cached.(providerCacheEntry); ok {
			if entry.APIKey == cfg.APIKey && len(entry.Models) > 0 {
				allModelNames := make([]string, 0, len(entry.Models))
				for _, m := range entry.Models {
					allModelNames = append(allModelNames, m.Model)
				}
				pulledModels := make([]statetype.ModelPullStatus, 0, len(entry.Models))
				for _, m := range entry.Models {
					if declaredModel, exists := declaredModels[m.Name]; exists {
						enhancedModel := m
						enhancedModel.Name = declaredModel.ID
						enhancedModel.Model = declaredModel.Model
						enhancedModel.ContextLength = declaredModel.ContextLength
						enhancedModel.CanChat = declaredModel.CanChat
						enhancedModel.CanEmbed = declaredModel.CanEmbed
						enhancedModel.CanPrompt = declaredModel.CanPrompt
						enhancedModel.CanStream = declaredModel.CanStream
						pulledModels = append(pulledModels, enhancedModel)
					} else if s.autoDiscoverModels {
						pulledModels = append(pulledModels, inferOpenAICapabilities(m.Model))
					}
				}
				stateInstance.Models = allModelNames
				stateInstance.PulledModels = pulledModels
				stateInstance.SetAPIKey(entry.APIKey)
				if len(declaredModels) > 0 && len(pulledModels) == 0 && !s.autoDiscoverModels {
					declaredMap := []string{}
					for k, n := range declaredModels {
						p := "model-data==nil"
						if n != nil {
							p = n.ID + " " + n.Model
						}
						declaredMap = append(declaredMap, k+":"+p)
					}
					stateInstance.Error = fmt.Sprintf("None of the declared models are available in the OpenAI API: declared models: %v \navailable models %s", strings.Join(declaredMap, ", "), allModelNames)
				}
				s.state.Store(backend.ID, stateInstance)
				return
			}
		}
	}

	// Prepare HTTP request
	client := &http.Client{Timeout: 10 * time.Second}
	reqURL := strings.TrimSuffix(backend.BaseURL, "/") + "/models"
	req, err := http.NewRequestWithContext(ctx, "GET", reqURL, nil)
	if err != nil {
		stateInstance.Error = fmt.Sprintf("Request creation failed: %v", err)
		s.state.Store(backend.ID, stateInstance)
		return
	}
	req.Header.Set("Authorization", "Bearer "+cfg.APIKey)

	// Execute request
	resp, err := client.Do(req)
	if err != nil {
		stateInstance.Error = fmt.Sprintf("HTTP request failed: %v", err)
		s.state.Store(backend.ID, stateInstance)
		return
	}
	defer resp.Body.Close()

	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		stateInstance.Error = fmt.Sprintf("Failed to read response body: %v", err)
		s.state.Store(backend.ID, stateInstance)
		return
	}

	// Handle non-200 responses
	if resp.StatusCode != http.StatusOK {
		stateInstance.Error = fmt.Sprintf("API returned %d: %s", resp.StatusCode, string(bodyBytes))
		s.state.Store(backend.ID, stateInstance)
		return
	}

	// Parse response
	var openAIResponse struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(bodyBytes, &openAIResponse); err != nil {
		stateInstance.Error = fmt.Sprintf("Response parsing failed: %v", err)
		s.state.Store(backend.ID, stateInstance)
		return
	}

	// Process all models for stateInstance.Models
	allModelNames := make([]string, 0, len(openAIResponse.Data))
	allModels := make([]statetype.ModelPullStatus, 0, len(openAIResponse.Data))

	// Process only declared models for PulledModels
	pulledModels := make([]statetype.ModelPullStatus, 0, len(openAIResponse.Data))

	for _, m := range openAIResponse.Data {
		allModelNames = append(allModelNames, m.ID)

		// Store minimal info for all models
		allModels = append(allModels, statetype.ModelPullStatus{
			Model: m.ID,
			Name:  m.ID,
		})

		// Include declared models (enhanced) or auto-discovered models (inferred caps).
		if declaredModel, exists := declaredModels[m.ID]; exists {
			modelResp := statetype.ModelPullStatus{
				Name:          declaredModel.ID,
				Model:         declaredModel.Model,
				ContextLength: declaredModel.ContextLength,
				CanChat:       declaredModel.CanChat,
				CanEmbed:      declaredModel.CanEmbed,
				CanPrompt:     declaredModel.CanPrompt,
				CanStream:     declaredModel.CanStream,
			}
			pulledModels = append(pulledModels, modelResp)
		} else if s.autoDiscoverModels {
			pulledModels = append(pulledModels, inferOpenAICapabilities(m.ID))
		}
	}

	// Update state
	stateInstance.Models = allModelNames
	stateInstance.PulledModels = pulledModels
	stateInstance.SetAPIKey(cfg.APIKey)
	if len(declaredModels) > 0 && len(pulledModels) == 0 && !s.autoDiscoverModels {
		declaredMap := []string{}
		for k, n := range declaredModels {
			p := "model-data==nil"
			if n != nil {
				p = n.ID + " " + n.Model
			}
			declaredMap = append(declaredMap, k+":"+p)
		}
		stateInstance.Error = fmt.Sprintf("None of the declared models are available in the OpenAI API: declared models: %v \navailable models %s", strings.Join(declaredMap, ", "), allModelNames)
	}

	s.state.Store(backend.ID, stateInstance)
	// Store successful result in cache (all models + pulled models)
	newEntry := providerCacheEntry{Models: allModels, APIKey: cfg.APIKey}
	if s.kvStore != nil {
		if exec, err := s.kvStore.Executor(ctx); err == nil {
			if data, err := json.Marshal(newEntry); err == nil {
				_ = exec.SetWithTTL(ctx, "prov:"+backend.ID, data, ProviderCacheDuration)
			}
		}
	} else {
		s.providerCache.Store(backend.ID, newEntry)
	}
}

func fetchGeminiModelInfo(ctx context.Context, baseURL, modelName, apiKey string, httpClient *http.Client) (*statetype.ModelPullStatus, error) {
	url := fmt.Sprintf("%s/v1beta/%s", baseURL, modelName)
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("X-Goog-Api-Key", apiKey)
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("API request failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("API returned error (%d): %s", resp.StatusCode, string(body))
	}
	var modelResponse struct {
		Name                       string   `json:"name"`
		InputTokenLimit            int      `json:"inputTokenLimit"`
		OutputTokenLimit           int      `json:"outputTokenLimit"`
		SupportedGenerationMethods []string `json:"supportedGenerationMethods"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&modelResponse); err != nil {
		return nil, fmt.Errorf("failed to parse response: %w", err)
	}
	// Determine capabilities from API response
	canChat := false
	canPrompt := false
	canEmbed := false
	canStream := false
	for _, method := range modelResponse.SupportedGenerationMethods {
		switch method {
		case "generateContent":
			canChat = true
			canPrompt = true
			canStream = true
		case "embedContent":
			canEmbed = true
		}
	}
	return &statetype.ModelPullStatus{
		Name:          modelName,
		Model:         modelName,
		ContextLength: modelResponse.InputTokenLimit,
		CanChat:       canChat,
		CanPrompt:     canPrompt,
		CanEmbed:      canEmbed,
		CanStream:     canStream,
	}, nil

}

// inferOpenAICapabilities assigns capabilities to an auto-discovered OpenAI model using
// name-based heuristics. The OpenAI /v1/models endpoint returns only id/created/owned_by —
// no capability metadata — so classification is done by model family prefix/suffix.
//
// Classification table (derived from the live OpenAI model list):
//
//	text-embedding-*                → CanEmbed only
//	*-instruct*, davinci-*, babbage-*  → CanPrompt only (completion, not chat)
//	dall-e-*, *-image-*, sora-*,
//	chatgpt-image-*                 → image generation, none of our caps
//	tts-*, *-tts*, whisper-*,
//	*-audio-*, *-realtime-*,
//	*-transcribe*, omni-moderation-* → audio/moderation, none of our caps
//	o1-*, o3-*, o4-*, gpt-* (chat)  → CanChat + CanPrompt + CanStream
//	everything else                 → CanChat=true (safe default, better than silent fail)
func inferOpenAICapabilities(id string) statetype.ModelPullStatus {
	lower := strings.ToLower(id)

	canChat, canEmbed, canPrompt, canStream := false, false, false, false

	switch {
	// Embed-only
	case strings.HasPrefix(lower, "text-embedding-"):
		canEmbed = true

	// Completion-only (prompt, not chat)
	case strings.Contains(lower, "-instruct"),
		strings.HasPrefix(lower, "davinci-"),
		strings.HasPrefix(lower, "babbage-"):
		canPrompt = true

	// Image / video generation — none of our caps
	case strings.HasPrefix(lower, "dall-e-"),
		strings.HasPrefix(lower, "sora-"),
		strings.HasPrefix(lower, "chatgpt-image-"),
		strings.Contains(lower, "-image-") && !strings.HasPrefix(lower, "gpt-image-"):
		// no caps

	// gpt-image-* lines (e.g. gpt-image-1, gpt-image-1-mini, gpt-image-1.5)
	case strings.HasPrefix(lower, "gpt-image-"):
		// no caps

	// Audio / TTS / realtime / transcription / moderation
	case strings.HasPrefix(lower, "tts-"),
		strings.HasSuffix(lower, "-tts"),
		strings.Contains(lower, "-tts-"),
		strings.HasPrefix(lower, "whisper-"),
		strings.Contains(lower, "-audio-"),
		strings.HasPrefix(lower, "gpt-audio"),
		strings.HasPrefix(lower, "gpt-realtime"),
		strings.Contains(lower, "-realtime-"),
		strings.Contains(lower, "-transcribe"),
		strings.HasPrefix(lower, "omni-"):
		// no caps

	// Chat models: gpt-* (general), o1/o3/o4 reasoning, gpt-5* families
	case strings.HasPrefix(lower, "gpt-"),
		strings.HasPrefix(lower, "o1"),
		strings.HasPrefix(lower, "o3"),
		strings.HasPrefix(lower, "o4"):
		canChat = true
		canPrompt = true
		canStream = true

	default:
		// Unknown model: default to chat so it doesn't silently disappear.
		canChat = true
		canPrompt = true
		canStream = true
	}

	return statetype.ModelPullStatus{
		Name:          id,
		Model:         id,
		ContextLength: 0, // unknown; llmresolver treats 0 as "don't filter on context"
		CanChat:       canChat,
		CanEmbed:      canEmbed,
		CanPrompt:     canPrompt,
		CanStream:     canStream,
	}
}

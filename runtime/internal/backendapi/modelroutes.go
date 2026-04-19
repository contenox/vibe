package backendapi

import (
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"

	apiframework "github.com/contenox/contenox/apiframework"
	"github.com/contenox/contenox/runtime/stateservice"
	"github.com/contenox/contenox/runtime/statetype"
)

func AddModelRoutes(mux *http.ServeMux, stateService stateservice.Service) {
	s := &service{stateService: stateService}

	mux.HandleFunc("GET /openai/v1/models", s.listModels)
	mux.HandleFunc("GET /openai/{chainID}/v1/models", s.listModels)
	mux.HandleFunc("GET /models", s.listInternal)
}

type service struct {
	stateService stateservice.Service
}

type ObservedModel struct {
	ID            string `json:"id" example:"mistral:instruct"`
	Model         string `json:"model" example:"mistral:instruct"`
	ContextLength int    `json:"contextLength" example:"8192"`
	CanChat       bool   `json:"canChat" example:"true"`
	CanEmbed      bool   `json:"canEmbed" example:"false"`
	CanPrompt     bool   `json:"canPrompt" example:"true"`
	CanStream     bool   `json:"canStream" example:"true"`
}

type OpenAIModel struct {
	ID      string `json:"id" example:"mistral:latest"`
	Object  string `json:"object" example:"mistral:latest"`
	Created int64  `json:"created" example:"1717020800"`
	OwnedBy string `json:"owned_by" example:"system"`
}

type OpenAICompatibleModelList struct {
	Object string        `json:"object" example:"list"`
	Data   []OpenAIModel `json:"data"`
}

// Lists all runtime-observed models in OpenAI-compatible format.
//
// Returns models observed on reachable backends as they would appear in OpenAI's /v1/models endpoint.
// The chainID parameter is currently unused.
func (s *service) listModels(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	limitStr := apiframework.GetQueryParam(r, "limit", "100", "The maximum number of items to return per page.")
	_ = apiframework.GetPathParam(r, "chainID", "The ID of the chain that links to the openAI completion API. Currently unused.")
	limit, err := parseObservedModelLimit(limitStr)
	if err != nil {
		_ = apiframework.Error(w, r, err, apiframework.ListOperation)
		return
	}

	internalModels, err := s.stateService.Get(ctx)
	if err != nil {
		_ = apiframework.Error(w, r, err, apiframework.ListOperation)
		return
	}

	observedModels := listObservedModels(internalModels)
	if limit < len(observedModels) {
		observedModels = observedModels[:limit]
	}

	openAIModels := make([]OpenAIModel, len(observedModels))
	for i, model := range observedModels {
		openAIModels[i] = OpenAIModel{
			ID:      model.Model,
			Object:  "model",
			Created: 0,
			OwnedBy: "runtime",
		}
	}

	response := OpenAICompatibleModelList{
		Object: "list",
		Data:   openAIModels,
	}

	apiframework.Encode(w, r, http.StatusOK, response) // @response backendapi.OpenAICompatibleModelList
}

// Lists all runtime-observed models in internal format.
//
// This endpoint returns observed model details merged across reachable backends.
// Intended for debugging and inventory views, not CRUD.
func (s *service) listInternal(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	limitStr := apiframework.GetQueryParam(r, "limit", "100", "The maximum number of items to return per page.")
	limit, err := parseObservedModelLimit(limitStr)
	if err != nil {
		_ = apiframework.Error(w, r, err, apiframework.ListOperation)
		return
	}

	states, err := s.stateService.Get(ctx)
	if err != nil {
		_ = apiframework.Error(w, r, err, apiframework.ListOperation)
		return
	}

	models := listObservedModels(states)
	if limit < len(models) {
		models = models[:limit]
	}

	_ = apiframework.Encode(w, r, http.StatusOK, models) // @response []backendapi.ObservedModel
}

func parseObservedModelLimit(limitStr string) (int, error) {
	limit := 100
	if limitStr == "" {
		return limit, nil
	}

	i, err := strconv.Atoi(limitStr)
	if err != nil {
		return 0, fmt.Errorf("%w: invalid limit format, expected integer", apiframework.ErrUnprocessableEntity)
	}
	if i < 1 {
		return 0, fmt.Errorf("%w: limit must be positive", apiframework.ErrUnprocessableEntity)
	}
	return i, nil
}

func listObservedModels(states []statetype.BackendRuntimeState) []ObservedModel {
	byName := map[string]ObservedModel{}

	for _, state := range sanitizeRuntimeStates(states) {
		for _, pulled := range state.PulledModels {
			name := strings.TrimSpace(pulled.Model)
			if name == "" {
				name = strings.TrimSpace(pulled.Name)
			}
			if name == "" {
				continue
			}

			model := byName[name]
			if model.ID == "" {
				model = ObservedModel{
					ID:    name,
					Model: name,
				}
			}

			if pulled.ContextLength > model.ContextLength {
				model.ContextLength = pulled.ContextLength
			}
			model.CanChat = model.CanChat || pulled.CanChat
			model.CanEmbed = model.CanEmbed || pulled.CanEmbed
			model.CanPrompt = model.CanPrompt || pulled.CanPrompt
			model.CanStream = model.CanStream || pulled.CanStream

			byName[name] = model
		}

	}

	names := make([]string, 0, len(byName))
	for name := range byName {
		names = append(names, name)
	}
	sort.Strings(names)

	models := make([]ObservedModel, 0, len(names))
	for _, name := range names {
		models = append(models, byName[name])
	}
	return models
}

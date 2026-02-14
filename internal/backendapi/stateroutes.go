package backendapi

import (
	"net/http"

	serverops "github.com/contenox/vibe/apiframework"
	"github.com/contenox/vibe/stateservice"
)

func AddStateRoutes(mux *http.ServeMux, stateService stateservice.Service) {
	s := &statemux{stateService: stateService}

	mux.HandleFunc("GET /state", s.list)
}

type statemux struct {
	stateService stateservice.Service
}

// Retrieves the current runtime state of all LLM backends.
//
// Includes connection status, loaded models, and error information.
// NOTE: This shows the physical state of backends, but the routing system only considers
// backends and models that are assigned to the same group. Resources not in groups are ignored
// for request processing even if they appear in this response.
func (s *statemux) list(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	internalModels, err := s.stateService.Get(ctx)
	if err != nil {
		_ = serverops.Error(w, r, err, serverops.ListOperation)
		return
	}
	serverops.Encode(w, r, http.StatusOK, internalModels) // @response []statetype.BackendRuntimeState
}

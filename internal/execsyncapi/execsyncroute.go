package execsyncapi

import (
	"net/http"

	"github.com/contenox/vibe/apiframework"
	"github.com/contenox/vibe/executor"
	"github.com/contenox/vibe/internal/eventdispatch"
)

// Add this struct near the other handler structs
type executorHandler struct {
	service    executor.ExecutorSyncTrigger
	dispatcher eventdispatch.Sync
}

// Add this function to register the executor routes
func AddExecutorRoutes(mux *http.ServeMux, service executor.ExecutorSyncTrigger, dispatcher eventdispatch.Sync) {
	e := &executorHandler{service: service, dispatcher: dispatcher}
	mux.HandleFunc("POST /executor/sync", e.triggerSync)
}

// Implement the handler method
func (e *executorHandler) triggerSync(w http.ResponseWriter, r *http.Request) {
	e.service.TriggerSync()
	err := e.dispatcher.Sync(r.Context())
	if err != nil {
		apiframework.Error(w, r, err, apiframework.ExecuteOperation)
		return
	}
	apiframework.Encode(w, r, http.StatusOK, "sync triggered") // @response string
}

package functionapi

import (
	"fmt"
	"net/http"
	"strconv"
	"time"

	serverops "github.com/contenox/vibe/apiframework"
	"github.com/contenox/vibe/functionservice"
	"github.com/contenox/vibe/functionstore"
)

// AddFunctionRoutes registers all function and event trigger API endpoints
func AddFunctionRoutes(mux *http.ServeMux, functionService functionservice.Service) {
	h := &functionHandler{service: functionService}

	// Function management endpoints
	mux.HandleFunc("POST /functions", h.createFunction)
	mux.HandleFunc("GET /functions", h.listFunctions)
	mux.HandleFunc("GET /functions/{name}", h.getFunction)
	mux.HandleFunc("PUT /functions/{name}", h.updateFunction)
	mux.HandleFunc("DELETE /functions/{name}", h.deleteFunction)

	// Event trigger management endpoints
	mux.HandleFunc("POST /event-triggers", h.createEventTrigger)
	mux.HandleFunc("GET /event-triggers", h.listEventTriggers)
	mux.HandleFunc("GET /event-triggers/{name}", h.getEventTrigger)
	mux.HandleFunc("PUT /event-triggers/{name}", h.updateEventTrigger)
	mux.HandleFunc("DELETE /event-triggers/{name}", h.deleteEventTrigger)
	mux.HandleFunc("GET /event-triggers/event-type/{eventType}", h.listEventTriggersByEventType)
	mux.HandleFunc("GET /event-triggers/function/{functionName}", h.listEventTriggersByFunction)
}

type functionHandler struct {
	service functionservice.Service
}

// Creates a new serverless function
//
// Functions contain executable JavaScript code that runs in a secure sandbox.
// After execution, functions can trigger chains for further processing.
func (h *functionHandler) createFunction(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	function, err := serverops.Decode[functionstore.Function](r) // @request functionstore.Function
	if err != nil {
		_ = serverops.Error(w, r, err, serverops.CreateOperation)
		return
	}

	if err := h.service.CreateFunction(ctx, &function); err != nil {
		_ = serverops.Error(w, r, err, serverops.CreateOperation)
		return
	}

	_ = serverops.Encode(w, r, http.StatusCreated, function) // @response functionstore.Function
}

// Lists all registered functions with pagination
//
// Returns functions in creation order, with the oldest functions first.
func (h *functionHandler) listFunctions(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	// Parse pagination parameters using the helper
	limitStr := serverops.GetQueryParam(r, "limit", "100", "The maximum number of items to return per page.")
	cursorStr := serverops.GetQueryParam(r, "cursor", "", "An optional RFC3339Nano timestamp to fetch the next page of results.")

	var cursor *time.Time
	if cursorStr != "" {
		t, err := time.Parse(time.RFC3339Nano, cursorStr)
		if err != nil {
			err = fmt.Errorf("%w: invalid cursor format, expected RFC3339Nano", serverops.ErrUnprocessableEntity)
			_ = serverops.Error(w, r, err, serverops.ListOperation)
			return
		}
		cursor = &t
	}

	limit, err := strconv.Atoi(limitStr)
	if err != nil {
		err = fmt.Errorf("%w: invalid limit format, expected integer", serverops.ErrUnprocessableEntity)
		_ = serverops.Error(w, r, err, serverops.ListOperation)
		return
	}

	functions, err := h.service.ListFunctions(ctx, cursor, limit)
	if err != nil {
		_ = serverops.Error(w, r, err, serverops.ListOperation)
		return
	}

	_ = serverops.Encode(w, r, http.StatusOK, functions) // @response []*functionstore.Function
}

// Retrieves details for a specific function
func (h *functionHandler) getFunction(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	name := serverops.GetPathParam(r, "name", "The unique name of the function.")
	if name == "" {
		_ = serverops.Error(w, r, fmt.Errorf("missing name parameter %w", serverops.ErrBadPathValue), serverops.GetOperation)
		return
	}

	function, err := h.service.GetFunction(ctx, name)
	if err != nil {
		_ = serverops.Error(w, r, err, serverops.GetOperation)
		return
	}

	_ = serverops.Encode(w, r, http.StatusOK, function) // @response functionstore.Function
}

// Updates an existing function configuration
//
// The name from the URL path overrides any name in the request body.
func (h *functionHandler) updateFunction(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	name := serverops.GetPathParam(r, "name", "The unique name of the function.")
	if name == "" {
		_ = serverops.Error(w, r, fmt.Errorf("missing name parameter %w", serverops.ErrBadPathValue), serverops.UpdateOperation)
		return
	}

	function, err := serverops.Decode[functionstore.Function](r) // @request functionstore.Function
	if err != nil {
		_ = serverops.Error(w, r, err, serverops.UpdateOperation)
		return
	}

	function.Name = name
	if err := h.service.UpdateFunction(ctx, &function); err != nil {
		_ = serverops.Error(w, r, err, serverops.UpdateOperation)
		return
	}

	_ = serverops.Encode(w, r, http.StatusOK, function) // @response functionstore.Function
}

// Deletes a function from the system
//
// Returns a simple confirmation message on success.
func (h *functionHandler) deleteFunction(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	name := serverops.GetPathParam(r, "name", "The unique name of the function.")
	if name == "" {
		_ = serverops.Error(w, r, fmt.Errorf("missing name parameter %w", serverops.ErrBadPathValue), serverops.DeleteOperation)
		return
	}

	if err := h.service.DeleteFunction(ctx, name); err != nil {
		_ = serverops.Error(w, r, err, serverops.DeleteOperation)
		return
	}

	_ = serverops.Encode(w, r, http.StatusOK, "function removed") // @response string
}

// Creates a new event trigger
//
// Event triggers listen for specific events and execute associated functions.
func (h *functionHandler) createEventTrigger(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	trigger, err := serverops.Decode[functionstore.EventTrigger](r) // @request functionstore.EventTrigger
	if err != nil {
		_ = serverops.Error(w, r, err, serverops.CreateOperation)
		return
	}

	if err := h.service.CreateEventTrigger(ctx, &trigger); err != nil {
		_ = serverops.Error(w, r, err, serverops.CreateOperation)
		return
	}

	_ = serverops.Encode(w, r, http.StatusCreated, trigger) // @response functionstore.EventTrigger
}

// Lists all event triggers with pagination
//
// Returns event triggers in creation order, with the oldest triggers first.
func (h *functionHandler) listEventTriggers(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	// Parse pagination parameters using the helper
	limitStr := serverops.GetQueryParam(r, "limit", "100", "The maximum number of items to return per page.")
	cursorStr := serverops.GetQueryParam(r, "cursor", "", "An optional RFC3339Nano timestamp to fetch the next page of results.")

	var cursor *time.Time
	if cursorStr != "" {
		t, err := time.Parse(time.RFC3339Nano, cursorStr)
		if err != nil {
			err = fmt.Errorf("%w: invalid cursor format, expected RFC3339Nano", serverops.ErrUnprocessableEntity)
			_ = serverops.Error(w, r, err, serverops.ListOperation)
			return
		}
		cursor = &t
	}

	limit, err := strconv.Atoi(limitStr)
	if err != nil {
		err = fmt.Errorf("%w: invalid limit format, expected integer", serverops.ErrUnprocessableEntity)
		_ = serverops.Error(w, r, err, serverops.ListOperation)
		return
	}

	triggers, err := h.service.ListEventTriggers(ctx, cursor, limit)
	if err != nil {
		_ = serverops.Error(w, r, err, serverops.ListOperation)
		return
	}

	_ = serverops.Encode(w, r, http.StatusOK, triggers) // @response []functionstore.EventTrigger
}

// Retrieves details for a specific event trigger
func (h *functionHandler) getEventTrigger(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	name := serverops.GetPathParam(r, "name", "The unique name of the event trigger.")
	if name == "" {
		_ = serverops.Error(w, r, fmt.Errorf("missing name parameter %w", serverops.ErrBadPathValue), serverops.GetOperation)
		return
	}

	trigger, err := h.service.GetEventTrigger(ctx, name)
	if err != nil {
		_ = serverops.Error(w, r, err, serverops.GetOperation)
		return
	}

	_ = serverops.Encode(w, r, http.StatusOK, trigger) // @response functionstore.EventTrigger
}

// Updates an existing event trigger configuration
//
// The name from the URL path overrides any name in the request body.
func (h *functionHandler) updateEventTrigger(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	name := serverops.GetPathParam(r, "name", "The unique name of the event trigger.")
	if name == "" {
		_ = serverops.Error(w, r, fmt.Errorf("missing name parameter %w", serverops.ErrBadPathValue), serverops.UpdateOperation)
		return
	}

	trigger, err := serverops.Decode[functionstore.EventTrigger](r) // @request functionstore.EventTrigger
	if err != nil {
		_ = serverops.Error(w, r, err, serverops.UpdateOperation)
		return
	}

	trigger.Name = name
	if err := h.service.UpdateEventTrigger(ctx, &trigger); err != nil {
		_ = serverops.Error(w, r, err, serverops.UpdateOperation)
		return
	}

	_ = serverops.Encode(w, r, http.StatusOK, trigger) // @response functionstore.EventTrigger
}

// Deletes an event trigger from the system
//
// Returns a simple confirmation message on success.
func (h *functionHandler) deleteEventTrigger(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	name := serverops.GetPathParam(r, "name", "The unique name of the event trigger.")
	if name == "" {
		_ = serverops.Error(w, r, fmt.Errorf("missing name parameter %w", serverops.ErrBadPathValue), serverops.DeleteOperation)
		return
	}

	if err := h.service.DeleteEventTrigger(ctx, name); err != nil {
		_ = serverops.Error(w, r, err, serverops.DeleteOperation)
		return
	}

	_ = serverops.Encode(w, r, http.StatusOK, "event trigger removed") // @response string
}

// Lists event triggers filtered by event type
//
// Returns all event triggers that listen for the specified event type.
func (h *functionHandler) listEventTriggersByEventType(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	eventType := serverops.GetPathParam(r, "eventType", "The event type to filter by.")
	if eventType == "" {
		_ = serverops.Error(w, r, fmt.Errorf("missing eventType parameter %w", serverops.ErrBadPathValue), serverops.ListOperation)
		return
	}

	triggers, err := h.service.ListEventTriggersByEventType(ctx, eventType)
	if err != nil {
		_ = serverops.Error(w, r, err, serverops.ListOperation)
		return
	}

	_ = serverops.Encode(w, r, http.StatusOK, triggers) // @response []*functionstore.EventTrigger
}

// Lists event triggers filtered by function name
//
// Returns all event triggers that execute the specified function.
func (h *functionHandler) listEventTriggersByFunction(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	functionName := serverops.GetPathParam(r, "functionName", "The function name to filter by.")
	if functionName == "" {
		_ = serverops.Error(w, r, fmt.Errorf("missing functionName parameter %w", serverops.ErrBadPathValue), serverops.ListOperation)
		return
	}

	triggers, err := h.service.ListEventTriggersByFunction(ctx, functionName)
	if err != nil {
		_ = serverops.Error(w, r, err, serverops.ListOperation)
		return
	}

	_ = serverops.Encode(w, r, http.StatusOK, triggers) // @response []*functionstore.EventTrigger
}

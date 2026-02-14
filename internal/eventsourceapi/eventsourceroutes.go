package eventsourceapi

import (
	"fmt"
	"net/http"
	"strconv"
	"time"

	serverops "github.com/contenox/vibe/apiframework"
	"github.com/contenox/vibe/eventsourceservice"
	"github.com/contenox/vibe/eventstore"
)

// AddEventSourceRoutes registers HTTP routes for event source operations.
func AddEventSourceRoutes(mux *http.ServeMux, service eventsourceservice.Service) {
	e := &eventSourceManager{service: service}

	// Write operations
	mux.HandleFunc("POST /events", e.appendEvent)

	// Read operations
	mux.HandleFunc("GET /events/aggregate", e.getEventsByAggregate)
	mux.HandleFunc("GET /events/type", e.getEventsByType)
	mux.HandleFunc("GET /events/source", e.getEventsBySource)
	mux.HandleFunc("GET /events/types", e.getEventTypesInRange)
	mux.HandleFunc("GET /raw-events/{nid}", e.getRawEvent)
	mux.HandleFunc("GET /raw-events", e.listRawEvents)
	mux.HandleFunc("POST /raw-events", e.appendRawEvent)

	// Delete operations
	mux.HandleFunc("DELETE /events/type", e.deleteEventsByTypeInRange)

	mux.HandleFunc("GET /events/stream/{eventType}", e.streamEvents)
}

type eventSourceManager struct {
	service eventsourceservice.Service
}

// Appends a new event to the event store.
//
// The event ID and CreatedAt will be auto-generated if not provided.
// Events must be within ±10 minutes of current server time.
func (e *eventSourceManager) appendEvent(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	event, err := serverops.Decode[eventstore.Event](r) // @request eventstore.Event
	if err != nil {
		_ = serverops.Error(w, r, err, serverops.CreateOperation)
		return
	}

	if err := e.service.AppendEvent(ctx, &event); err != nil {
		_ = serverops.Error(w, r, err, serverops.CreateOperation)
		return
	}

	_ = serverops.Encode(w, r, http.StatusCreated, event) // @response eventstore.Event
}

// Retrieves events for a specific aggregate within a time range.
//
// Useful for rebuilding aggregate state or auditing changes.
func (e *eventSourceManager) getEventsByAggregate(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	eventType := serverops.GetQueryParam(r, "event_type", "", "The type of event to filter by.")
	aggregateType := serverops.GetQueryParam(r, "aggregate_type", "", "The aggregate type (e.g., 'user', 'order').")
	aggregateID := serverops.GetQueryParam(r, "aggregate_id", "", "The unique ID of the aggregate.")
	fromStr := serverops.GetQueryParam(r, "from", time.Now().UTC().Add(-24*time.Hour).Format(time.RFC3339), "Start time in RFC3339 format.")
	toStr := serverops.GetQueryParam(r, "to", time.Now().UTC().Format(time.RFC3339), "End time in RFC3339 format.")
	limitStr := serverops.GetQueryParam(r, "limit", "100", "Maximum number of events to return.")

	if eventType == "" {
		_ = serverops.Error(w, r, fmt.Errorf("event_type is required %w", serverops.ErrUnprocessableEntity), serverops.ListOperation)
		return
	}
	if aggregateType == "" {
		_ = serverops.Error(w, r, fmt.Errorf("aggregate_type is required %w", serverops.ErrUnprocessableEntity), serverops.ListOperation)
		return
	}
	if aggregateID == "" {
		_ = serverops.Error(w, r, fmt.Errorf("aggregate_id is required %w", serverops.ErrUnprocessableEntity), serverops.ListOperation)
		return
	}

	from, err := time.Parse(time.RFC3339, fromStr)
	if err != nil {
		_ = serverops.Error(w, r, fmt.Errorf("invalid 'from' time format, expected RFC3339 %w", serverops.ErrUnprocessableEntity), serverops.ListOperation)
		return
	}

	to, err := time.Parse(time.RFC3339, toStr)
	if err != nil {
		_ = serverops.Error(w, r, fmt.Errorf("invalid 'to' time format, expected RFC3339 %w", serverops.ErrUnprocessableEntity), serverops.ListOperation)
		return
	}

	limit, err := strconv.Atoi(limitStr)
	if err != nil || limit <= 0 {
		_ = serverops.Error(w, r, fmt.Errorf("invalid limit, must be positive integer %w", serverops.ErrUnprocessableEntity), serverops.ListOperation)
		return
	}

	events, err := e.service.GetEventsByAggregate(ctx, eventType, from, to, aggregateType, aggregateID, limit)
	if err != nil {
		_ = serverops.Error(w, r, err, serverops.ListOperation)
		return
	}

	_ = serverops.Encode(w, r, http.StatusOK, events) // @response []eventstore.Event
}

// Retrieves events of a specific type within a time range.
//
// Useful for cross-aggregate analysis or system-wide event monitoring.
func (e *eventSourceManager) getEventsByType(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	eventType := serverops.GetQueryParam(r, "event_type", "", "The type of event to filter by.")
	fromStr := serverops.GetQueryParam(r, "from", time.Now().UTC().Add(-24*time.Hour).Format(time.RFC3339), "Start time in RFC3339 format.")
	toStr := serverops.GetQueryParam(r, "to", time.Now().UTC().Format(time.RFC3339), "End time in RFC3339 format.")
	limitStr := serverops.GetQueryParam(r, "limit", "100", "Maximum number of events to return.")

	if eventType == "" {
		_ = serverops.Error(w, r, fmt.Errorf("event_type is required %w", serverops.ErrUnprocessableEntity), serverops.ListOperation)
		return
	}

	from, err := time.Parse(time.RFC3339, fromStr)
	if err != nil {
		_ = serverops.Error(w, r, fmt.Errorf("invalid 'from' time format, expected RFC3339 %w", serverops.ErrUnprocessableEntity), serverops.ListOperation)
		return
	}

	to, err := time.Parse(time.RFC3339, toStr)
	if err != nil {
		_ = serverops.Error(w, r, fmt.Errorf("invalid 'to' time format, expected RFC3339 %w", serverops.ErrUnprocessableEntity), serverops.ListOperation)
		return
	}

	limit, err := strconv.Atoi(limitStr)
	if err != nil || limit <= 0 {
		_ = serverops.Error(w, r, fmt.Errorf("invalid limit, must be positive integer %w", serverops.ErrUnprocessableEntity), serverops.ListOperation)
		return
	}

	events, err := e.service.GetEventsByType(ctx, eventType, from, to, limit)
	if err != nil {
		_ = serverops.Error(w, r, err, serverops.ListOperation)
		return
	}

	_ = serverops.Encode(w, r, http.StatusOK, events) // @response []eventstore.Event
}

// Retrieves events from a specific source within a time range.
//
// Useful for auditing or monitoring events from specific subsystems.
func (e *eventSourceManager) getEventsBySource(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	eventType := serverops.GetQueryParam(r, "event_type", "", "The type of event to filter by.")
	eventSource := serverops.GetQueryParam(r, "event_source", "", "The source system that generated the event.")
	fromStr := serverops.GetQueryParam(r, "from", time.Now().UTC().Add(-24*time.Hour).Format(time.RFC3339), "Start time in RFC3339 format.")
	toStr := serverops.GetQueryParam(r, "to", time.Now().UTC().Format(time.RFC3339), "End time in RFC3339 format.")
	limitStr := serverops.GetQueryParam(r, "limit", "100", "Maximum number of events to return.")

	if eventType == "" {
		_ = serverops.Error(w, r, fmt.Errorf("event_type is required %w", serverops.ErrUnprocessableEntity), serverops.ListOperation)
		return
	}
	if eventSource == "" {
		_ = serverops.Error(w, r, fmt.Errorf("event_source is required %w", serverops.ErrUnprocessableEntity), serverops.ListOperation)
		return
	}

	from, err := time.Parse(time.RFC3339, fromStr)
	if err != nil {
		_ = serverops.Error(w, r, fmt.Errorf("invalid 'from' time format, expected RFC3339 %w", serverops.ErrUnprocessableEntity), serverops.ListOperation)
		return
	}

	to, err := time.Parse(time.RFC3339, toStr)
	if err != nil {
		_ = serverops.Error(w, r, fmt.Errorf("invalid 'to' time format, expected RFC3339 %w", serverops.ErrUnprocessableEntity), serverops.ListOperation)
		return
	}

	limit, err := strconv.Atoi(limitStr)
	if err != nil || limit <= 0 {
		_ = serverops.Error(w, r, fmt.Errorf("invalid limit, must be positive integer %w", serverops.ErrUnprocessableEntity), serverops.ListOperation)
		return
	}

	events, err := e.service.GetEventsBySource(ctx, eventType, from, to, eventSource, limit)
	if err != nil {
		_ = serverops.Error(w, r, err, serverops.ListOperation)
		return
	}

	_ = serverops.Encode(w, r, http.StatusOK, events) // @response []eventstore.Event
}

// Lists distinct event types that occurred within a time range.
//
// Useful for discovery or building event type filters in UIs.
func (e *eventSourceManager) getEventTypesInRange(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	fromStr := serverops.GetQueryParam(r, "from", time.Now().UTC().Add(-24*time.Hour).Format(time.RFC3339), "Start time in RFC3339 format.")
	toStr := serverops.GetQueryParam(r, "to", time.Now().UTC().Format(time.RFC3339), "End time in RFC3339 format.")
	limitStr := serverops.GetQueryParam(r, "limit", "100", "Maximum number of event types to return.")

	from, err := time.Parse(time.RFC3339, fromStr)
	if err != nil {
		_ = serverops.Error(w, r, fmt.Errorf("invalid 'from' time format, expected RFC3339 %w", serverops.ErrUnprocessableEntity), serverops.ListOperation)
		return
	}

	to, err := time.Parse(time.RFC3339, toStr)
	if err != nil {
		_ = serverops.Error(w, r, fmt.Errorf("invalid 'to' time format, expected RFC3339 %w", serverops.ErrUnprocessableEntity), serverops.ListOperation)
		return
	}

	limit, err := strconv.Atoi(limitStr)
	if err != nil || limit <= 0 {
		_ = serverops.Error(w, r, fmt.Errorf("invalid limit, must be positive integer %w", serverops.ErrUnprocessableEntity), serverops.ListOperation)
		return
	}

	eventTypes, err := e.service.GetEventTypesInRange(ctx, from, to, limit)
	if err != nil {
		_ = serverops.Error(w, r, err, serverops.ListOperation)
		return
	}

	_ = serverops.Encode(w, r, http.StatusOK, eventTypes) // @response []string
}

// Deletes all events of a specific type within a time range.
//
// USE WITH CAUTION — this is a destructive operation.
// Typically used for GDPR compliance or cleaning up test data.
func (e *eventSourceManager) deleteEventsByTypeInRange(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	eventType := serverops.GetQueryParam(r, "event_type", "", "The type of event to delete.")
	fromStr := serverops.GetQueryParam(r, "from", "", "Start time in RFC3339 format.")
	toStr := serverops.GetQueryParam(r, "to", "", "End time in RFC3339 format.")

	if eventType == "" {
		_ = serverops.Error(w, r, fmt.Errorf("event_type is required %w", serverops.ErrUnprocessableEntity), serverops.DeleteOperation)
		return
	}
	if fromStr == "" {
		_ = serverops.Error(w, r, fmt.Errorf("'from' is required %w", serverops.ErrUnprocessableEntity), serverops.DeleteOperation)
		return
	}
	if toStr == "" {
		_ = serverops.Error(w, r, fmt.Errorf("'to' is required %w", serverops.ErrUnprocessableEntity), serverops.DeleteOperation)
		return
	}

	from, err := time.Parse(time.RFC3339, fromStr)
	if err != nil {
		_ = serverops.Error(w, r, fmt.Errorf("invalid 'from' time format, expected RFC3339 %w", serverops.ErrUnprocessableEntity), serverops.DeleteOperation)
		return
	}

	to, err := time.Parse(time.RFC3339, toStr)
	if err != nil {
		_ = serverops.Error(w, r, fmt.Errorf("invalid 'to' time format, expected RFC3339 %w", serverops.ErrUnprocessableEntity), serverops.DeleteOperation)
		return
	}

	if err := e.service.DeleteEventsByTypeInRange(ctx, eventType, from, to); err != nil {
		_ = serverops.Error(w, r, err, serverops.DeleteOperation)
		return
	}

	_ = serverops.Encode(w, r, http.StatusOK, "events deleted") // @response string
}

// Streams events of a specific type in real-time using Server-Sent Events (SSE)
//
// This endpoint provides real-time event streaming for the specified event type.
// Clients will receive new events as they are appended to the event store.
//
// --- SSE Streaming ---
// The endpoint streams events using Server-Sent Events (SSE) format.
// Each event is sent as a JSON object in the data field.
//
// Example event stream:
// data: {"id":"evt_123","event_type":"user_created","aggregate_type":"user","aggregate_id":"usr_456","version":1,"data":{"name":"John Doe"},"created_at":"2023-01-01T00:00:00Z"}
//
// data: {"id":"evt_124","event_type":"user_updated","aggregate_type":"user","aggregate_id":"usr_456","version":2,"data":{"name":"Jane Doe"},"created_at":"2023-01-01T00:01:00Z"}
func (e *eventSourceManager) streamEvents(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	eventType := serverops.GetPathParam(r, "eventType", "The type of events to stream.")
	if eventType == "" {
		_ = serverops.Error(w, r, fmt.Errorf("event_type is required %w", serverops.ErrUnprocessableEntity), serverops.ListOperation)
		return
	}
	// Set headers for Server-Sent Events (SSE)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("Access-Control-Allow-Origin", "*")

	flusher, ok := w.(http.Flusher)
	if !ok {
		_ = serverops.Error(w, r, fmt.Errorf("streaming unsupported"), serverops.ListOperation)
		return
	}

	// Create a channel to receive events
	eventCh := make(chan []byte, 10)

	// Subscribe to events using the service
	// Note: This requires adding a SubscribeToEvents method to the eventsourceservice.Service interface
	sub, err := e.service.SubscribeToEvents(ctx, eventType, eventCh)
	if err != nil {
		_ = serverops.Error(w, r, err, serverops.ListOperation)
		return
	}
	defer sub.Unsubscribe()

	// Send initial message to establish connection
	fmt.Fprintf(w, "data: Connected to event stream for type: %s\n\n", eventType)
	flusher.Flush()

	// Listen for events and stream them to the client
	for {
		select {
		case eventData := <-eventCh:
			fmt.Fprintf(w, "data: %s\n\n", eventData)
			flusher.Flush()
		case <-ctx.Done():
			// Client disconnected
			return
		case <-r.Context().Done():
			// Request context cancelled
			return
		}
	}
}

// Retrieves a raw event by numeric ID (NID) within a time range.
//
// This is useful for inspecting original payloads before mapping,
// or for preparing replay operations.
func (e *eventSourceManager) getRawEvent(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	nidStr := serverops.GetPathParam(r, "nid", "Numeric ID of the raw event")
	if nidStr == "" {
		_ = serverops.Error(w, r, fmt.Errorf("nid query parameter is required %w", serverops.ErrUnprocessableEntity), serverops.ListOperation)
		return
	}

	var nid int64
	if _, err := fmt.Sscan(nidStr, &nid); err != nil {
		_ = serverops.Error(w, r, fmt.Errorf("invalid nid format, must be integer %w", serverops.ErrUnprocessableEntity), serverops.ListOperation)
		return
	}

	fromStr := serverops.GetQueryParam(r, "from", "", "Start time in RFC3339 format")
	if fromStr == "" {
		_ = serverops.Error(w, r, fmt.Errorf("'from' query parameter is required %w", serverops.ErrUnprocessableEntity), serverops.ListOperation)
		return
	}

	toStr := serverops.GetQueryParam(r, "to", "", "End time in RFC3339 format")
	if toStr == "" {
		_ = serverops.Error(w, r, fmt.Errorf("'to' query parameter is required %w", serverops.ErrUnprocessableEntity), serverops.ListOperation)
		return
	}

	from, err := time.Parse(time.RFC3339, fromStr)
	if err != nil {
		_ = serverops.Error(w, r, fmt.Errorf("invalid 'from' time format, expected RFC3339 %w", serverops.ErrUnprocessableEntity), serverops.ListOperation)
		return
	}

	to, err := time.Parse(time.RFC3339, toStr)
	if err != nil {
		_ = serverops.Error(w, r, fmt.Errorf("invalid 'to' time format, expected RFC3339 %w", serverops.ErrUnprocessableEntity), serverops.ListOperation)
		return
	}

	if from.After(to) {
		_ = serverops.Error(w, r, fmt.Errorf("'from' cannot be after 'to' %w", serverops.ErrUnprocessableEntity), serverops.ListOperation)
		return
	}

	rawEvent, err := e.service.GetRawEvent(ctx, from, to, nid)
	if err != nil {
		_ = serverops.Error(w, r, err, serverops.ListOperation)
		return
	}

	_ = serverops.Encode(w, r, http.StatusOK, rawEvent) // @response eventstore.RawEvent
}

// Lists raw events within a time range.
//
// Useful for debugging, auditing, or preparing replay operations.
// Returns events in descending order of received_at.
func (e *eventSourceManager) listRawEvents(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	fromStr := serverops.GetQueryParam(r, "from", time.Now().UTC().Add(-24*time.Hour).Format(time.RFC3339), "Start time in RFC3339 format.")
	toStr := serverops.GetQueryParam(r, "to", time.Now().UTC().Format(time.RFC3339), "End time in RFC3339 format.")
	limitStr := serverops.GetQueryParam(r, "limit", "100", "Maximum number of raw events to return.")

	from, err := time.Parse(time.RFC3339, fromStr)
	if err != nil {
		_ = serverops.Error(w, r, fmt.Errorf("invalid 'from' time format, expected RFC3339 %w", serverops.ErrUnprocessableEntity), serverops.ListOperation)
		return
	}

	to, err := time.Parse(time.RFC3339, toStr)
	if err != nil {
		_ = serverops.Error(w, r, fmt.Errorf("invalid 'to' time format, expected RFC3339 %w", serverops.ErrUnprocessableEntity), serverops.ListOperation)
		return
	}

	limit, err := strconv.Atoi(limitStr)
	if err != nil || limit <= 0 {
		_ = serverops.Error(w, r, fmt.Errorf("invalid limit, must be positive integer %w", serverops.ErrUnprocessableEntity), serverops.ListOperation)
		return
	}

	if limit > 1000 {
		_ = serverops.Error(w, r, fmt.Errorf("limit cannot exceed 1000 %w", serverops.ErrUnprocessableEntity), serverops.ListOperation)
		return
	}

	if from.After(to) {
		_ = serverops.Error(w, r, fmt.Errorf("'from' cannot be after 'to' %w", serverops.ErrUnprocessableEntity), serverops.ListOperation)
		return
	}

	rawEvents, err := e.service.ListRawEvents(ctx, from, to, limit)
	if err != nil {
		_ = serverops.Error(w, r, err, serverops.ListOperation)
		return
	}

	_ = serverops.Encode(w, r, http.StatusOK, rawEvents) // @response []*eventstore.RawEvent
}

// Ingests a raw event into the event source.
//
// This handler should not be used directly.
func (e *eventSourceManager) appendRawEvent(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	rawEvent, err := serverops.Decode[eventstore.RawEvent](r) // @request eventstore.RawEvent
	if err != nil {
		_ = serverops.Error(w, r, err, serverops.CreateOperation)
		return
	}

	if err := e.service.AppendRawEvent(ctx, &rawEvent); err != nil {
		_ = serverops.Error(w, r, err, serverops.CreateOperation)
		return
	}

	_ = serverops.Encode(w, r, http.StatusCreated, rawEvent) // @response eventstore.RawEvent
}

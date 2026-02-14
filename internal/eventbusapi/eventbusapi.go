package eventbridgeapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/contenox/vibe/eventbridgeservice"
	"github.com/contenox/vibe/eventstore"

	serverops "github.com/contenox/vibe/apiframework"
)

// AddEventBridgeRoutes registers HTTP routes for event bridge operations
func AddEventBridgeRoutes(mux *http.ServeMux, bridgeService eventbridgeservice.Service) {
	h := &eventBridgeHandler{service: bridgeService}

	// Main ingestion endpoint - applies mapping configuration to incoming events
	mux.HandleFunc("POST /ingest", h.ingestEvent)

	// Sync endpoint to refresh mapping cache
	mux.HandleFunc("POST /sync", h.syncMappings)

	// Replay a raw event by NID to re-emit its domain event
	mux.HandleFunc("POST /replay", h.replayEvent)
}

type eventBridgeHandler struct {
	service eventbridgeservice.Service
}

// IngestEvent processes incoming events by applying mapping configuration
//
// This endpoint transforms raw payloads into structured events using the mapping
// configuration specified by the path query parameter. The mapping defines how to extract
// event properties like aggregate_id, event_type, etc. from the incoming data.
//
// The path query parameter corresponds to a pre-configured mapping that specifies:
// - How to extract the event type from the payload
// - How to extract the aggregate ID and type
// - How to handle metadata mapping
// - Field extraction rules for event properties
func (h *eventBridgeHandler) ingestEvent(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	path := serverops.GetQueryParam(r, "path", "", "The mapping configuration path to apply")
	if path == "" {
		_ = serverops.Error(w, r, fmt.Errorf("path query parameter is required: %w", serverops.ErrBadPathValue), serverops.CreateOperation)
		return
	}

	// Decode the incoming payload
	var payload map[string]interface{}
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		_ = serverops.Error(w, r, fmt.Errorf("invalid JSON payload: %w", err), serverops.CreateOperation)
		return
	}

	headers := make(map[string]string)
	for k, v := range headers {
		headers[k] = fmt.Sprint(v[0])
	}

	event := &eventstore.RawEvent{
		ID:         generateEventID(),
		ReceivedAt: time.Now().UTC(),
		Path:       path,
		Payload:    payload,
		Headers:    headers,
	}

	// Ingest the transformed event
	if err := h.service.Ingest(ctx, event); err != nil {
		_ = serverops.Error(w, r, fmt.Errorf("failed to ingest event: %w", err), serverops.CreateOperation)
		return
	}

	_ = serverops.Encode(w, r, http.StatusCreated, event) // @response eventstore.Event
}

// replayEvent replays a raw event by NID to re-emit its corresponding domain event
//
// This endpoint fetches a raw event by its numeric ID (NID) and time range,
// applies the current mapping configuration, and appends the resulting domain event.
func (h *eventBridgeHandler) replayEvent(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	nidStr := serverops.GetQueryParam(r, "nid", "", "Numeric ID of the raw event")
	if nidStr == "" {
		_ = serverops.Error(w, r, fmt.Errorf("nid query parameter is required: %w", serverops.ErrBadPathValue), serverops.CreateOperation)
		return
	}

	var nid int64
	if _, err := fmt.Sscan(nidStr, &nid); err != nil {
		_ = serverops.Error(w, r, fmt.Errorf("invalid nid format: %w", err), serverops.CreateOperation)
		return
	}

	fromStr := serverops.GetQueryParam(r, "from", "", "Start time (RFC3339)")
	if fromStr == "" {
		_ = serverops.Error(w, r, fmt.Errorf("from query parameter is required: %w", serverops.ErrBadPathValue), serverops.CreateOperation)
		return
	}

	from, err := time.Parse(time.RFC3339, fromStr)
	if err != nil {
		_ = serverops.Error(w, r, fmt.Errorf("invalid from time format: %w", err), serverops.CreateOperation)
		return
	}

	toStr := serverops.GetQueryParam(r, "to", "", "End time (RFC3339)")
	if toStr == "" {
		_ = serverops.Error(w, r, fmt.Errorf("to query parameter is required: %w", serverops.ErrBadPathValue), serverops.CreateOperation)
		return
	}

	to, err := time.Parse(time.RFC3339, toStr)
	if err != nil {
		_ = serverops.Error(w, r, fmt.Errorf("invalid to time format: %w", err), serverops.CreateOperation)
		return
	}

	if from.After(to) {
		_ = serverops.Error(w, r, fmt.Errorf("'from' cannot be after 'to': %w", serverops.ErrBadPathValue), serverops.CreateOperation)
		return
	}

	if err := h.service.ReplayEvent(ctx, from, to, nid); err != nil {
		_ = serverops.Error(w, r, fmt.Errorf("failed to replay event: %w", err), serverops.CreateOperation)
		return
	}

	_ = serverops.Encode(w, r, http.StatusAccepted, "event replayed") // @response string
}

// generateEventID creates a unique event identifier
func generateEventID() string {
	return fmt.Sprintf("evt_%d", time.Now().UnixNano())
}

// SyncMappings refreshes the mapping cache from the underlying storage
func (h *eventBridgeHandler) syncMappings(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	if err := h.service.Sync(ctx); err != nil {
		_ = serverops.Error(w, r, fmt.Errorf("failed to sync mappings: %w", err), serverops.UpdateOperation)
		return
	}

	_ = serverops.Encode(w, r, http.StatusOK, "mappings synchronized") // @response string
}

// ListMappings returns all configured event mappings
func (h *eventBridgeHandler) listMappings(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	mappings, err := h.service.ListMappings(ctx)
	if err != nil {
		_ = serverops.Error(w, r, err, serverops.ListOperation)
		return
	}

	_ = serverops.Encode(w, r, http.StatusOK, mappings) // @response []eventstore.MappingConfig
}

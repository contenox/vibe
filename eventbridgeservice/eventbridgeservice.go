package eventbridgeservice

import (
	"context"
	"encoding/json"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/PaesslerAG/jsonpath"
	"github.com/contenox/vibe/apiframework"
	"github.com/contenox/vibe/eventmappingservice"
	"github.com/contenox/vibe/eventsourceservice"
	"github.com/contenox/vibe/eventstore"
)

var (
	ErrMappingNotFound = fmt.Errorf("mapping not found: %w", apiframework.ErrNotFound)
)

type Service interface {
	Bus
	ListMappings(ctx context.Context) ([]eventstore.MappingConfig, error)
	GetMapping(ctx context.Context, path string) (*eventstore.MappingConfig, error)
	Renderer
}

type Renderer interface {
	Sync(ctx context.Context) error
}

type Bus interface {
	Ingest(ctx context.Context, event ...*eventstore.RawEvent) error
	ReplayEvent(ctx context.Context, from, to time.Time, nid int64) error
}

type eventBridge struct {
	eventMapping    eventmappingservice.Service
	eventsource     eventsourceservice.Service
	mappingCache    atomic.Pointer[map[string]*eventstore.MappingConfig]
	lastSync        atomic.Int64 // Unix nanoseconds
	syncInProgress  atomic.Bool
	callInitialSync atomic.Bool
	syncInterval    time.Duration
}

// New creates a new eventBridge instance with initial synchronization
func New(
	eventMapping eventmappingservice.Service,
	eventsource eventsourceservice.Service,
	syncInterval time.Duration,
) Service {
	bridge := &eventBridge{
		eventMapping:    eventMapping,
		eventsource:     eventsource,
		syncInterval:    syncInterval,
		callInitialSync: atomic.Bool{},
		syncInProgress:  atomic.Bool{},
	}

	// Initialize with empty map
	emptyCache := make(map[string]*eventstore.MappingConfig)
	bridge.mappingCache.Store(&emptyCache)
	bridge.callInitialSync.Store(true)

	return bridge
}

// GetMapping implements Service
func (e *eventBridge) GetMapping(ctx context.Context, path string) (*eventstore.MappingConfig, error) {
	if path == "" {
		return nil, fmt.Errorf("path is required")
	}

	// Ensure cache is up to date
	if _, err := e.syncMappings(ctx, false); err != nil {
		return nil, err
	}

	cache := *e.mappingCache.Load()
	if mapping, exists := cache[path]; exists {
		return mapping, nil
	}

	return nil, ErrMappingNotFound
}

// ReplayEvent implements Bus
func (e *eventBridge) ReplayEvent(ctx context.Context, from, to time.Time, nid int64) error {
	// 1. Retrieve the raw event
	rawEvent, err := e.eventsource.GetRawEvent(ctx, from, to, nid)
	if err != nil {
		return fmt.Errorf("failed to get raw event (nid=%d): %w", nid, err)
	}

	// 2. Get mapping for the raw event's path
	mapping, err := e.GetMapping(ctx, rawEvent.Path)
	if err != nil {
		return fmt.Errorf("failed to get mapping for path %s: %w", rawEvent.Path, err)
	}

	// 3. Apply mapping to produce domain event
	domainEvent, err := e.applyMapping(mapping, rawEvent.Payload, rawEvent.Headers)
	if err != nil {
		return fmt.Errorf("failed to apply mapping to raw event (nid=%d, path=%s): %w", nid, rawEvent.Path, err)
	}

	// 4. Append the domain event (do NOT re-append raw event)
	if err := e.eventsource.AppendEvent(ctx, domainEvent); err != nil {
		return fmt.Errorf("failed to append replayed domain event: %w", err)
	}

	return nil
}

// ListMappings implements Service
func (e *eventBridge) ListMappings(ctx context.Context) ([]eventstore.MappingConfig, error) {
	// Ensure cache is up to date
	cache, err := e.syncMappings(ctx, false)
	if err != nil {
		return nil, err
	}

	mappings := make([]eventstore.MappingConfig, 0, len(cache))
	for _, mapping := range cache {
		if mapping != nil {
			mappings = append(mappings, *mapping)
		}
	}

	return mappings, nil
}

// syncMappings synchronizes the mapping cache with the event mapping service
// It only performs I/O operations when necessary and uses atomic flags to prevent redundant operations
func (e *eventBridge) syncMappings(ctx context.Context, force bool) (map[string]*eventstore.MappingConfig, error) {
	// Check if we need to sync
	lastSync := time.Unix(0, e.lastSync.Load())
	needSync := force || e.callInitialSync.Load() || time.Since(lastSync) > e.syncInterval

	if needSync && e.syncInProgress.CompareAndSwap(false, true) {
		defer e.syncInProgress.Store(false)

		mappings, err := e.eventMapping.ListMappings(ctx)
		if err != nil {
			return nil, fmt.Errorf("failed to list mappings: %w", err)
		}

		// Build new cache
		newCache := make(map[string]*eventstore.MappingConfig)
		for _, mapping := range mappings {
			if mapping != nil && mapping.Path != "" {
				newCache[mapping.Path] = mapping
			}
		}

		// Atomically update cache
		e.mappingCache.Store(&newCache)
		e.lastSync.Store(time.Now().UnixNano())
		e.callInitialSync.Store(false)

		return newCache, nil
	}

	// If no sync needed or sync in progress, return current cache
	return *e.mappingCache.Load(), nil
}

// Sync implements Renderer interface
func (e *eventBridge) Sync(ctx context.Context) error {
	_, err := e.syncMappings(ctx, true)
	return err
}

// Ingest implements Bus interface
func (e *eventBridge) Ingest(ctx context.Context, events ...*eventstore.RawEvent) error {
	for i := range events {
		if err := e.eventsource.AppendRawEvent(ctx, events[i]); err != nil {
			return fmt.Errorf("failed to append raw event: %w", err)
		}
		mapping, err := e.GetMapping(ctx, events[i].Path)
		if err != nil {
			return fmt.Errorf("failed to get mapping for path %s: %w", events[i].Path, err)
		}
		ev, err := e.applyMapping(mapping, events[i].Payload, events[i].Headers)
		if err != nil {
			return fmt.Errorf("failed to apply mapping for path %s: %w", events[i].Path, err)
		}
		// Pass by pointer as AppendEvent expects *Event
		if err := e.eventsource.AppendEvent(ctx, ev); err != nil {
			return fmt.Errorf("failed to append event: %w", err)
		}
	}
	return nil
}

// applyMapping transforms a raw payload into a structured event using the mapping configuration
func (e *eventBridge) applyMapping(mapping *eventstore.MappingConfig, payload map[string]interface{}, headers map[string]string) (*eventstore.Event, error) {
	event := &eventstore.Event{
		ID:        generateEventID(),
		CreatedAt: time.Now().UTC(),
		Version:   mapping.Version,
	}

	// Extract event type - either from field or use fixed value
	event.EventType = mapping.EventType
	if mapping.EventTypeField != "" {
		if eventType, ok := getFieldValue(payload, mapping.EventTypeField); ok {
			event.EventType = fmt.Sprintf("%v", eventType)
		}
	}

	// Extract event source - either from field or use fixed value
	event.EventSource = mapping.EventSource
	if mapping.EventSourceField != "" {
		if eventSource, ok := getFieldValue(payload, mapping.EventSourceField); ok {
			event.EventSource = fmt.Sprintf("%v", eventSource)
		}
	}

	// Extract aggregate ID - required field
	if mapping.AggregateIDField != "" {
		if aggregateID, ok := getFieldValue(payload, mapping.AggregateIDField); ok {
			event.AggregateID = fmt.Sprintf("%v", aggregateID)
		} else {
			return nil, fmt.Errorf("aggregate ID field '%s' not found in payload", mapping.AggregateIDField)
		}
	} else {
		return nil, fmt.Errorf("aggregate ID field mapping is required")
	}

	// Extract or use fixed aggregate type
	if mapping.AggregateTypeField != "" {
		if aggregateType, ok := getFieldValue(payload, mapping.AggregateTypeField); ok {
			event.AggregateType = fmt.Sprintf("%v", aggregateType)
		} else {
			return nil, fmt.Errorf("aggregate type field '%s' not found in payload", mapping.AggregateTypeField)
		}
	} else if mapping.AggregateType != "" {
		event.AggregateType = mapping.AggregateType
	} else {
		return nil, fmt.Errorf("aggregate type field mapping is required")
	}

	// Set the payload data
	data, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal event data: %w", err)
	}
	event.Data = data

	// Extract metadata if mapping specified
	if len(mapping.MetadataMapping) > 0 {
		metadata := make(map[string]interface{})
		for metaKey, fieldPath := range mapping.MetadataMapping {
			if value, ok := getFieldValue(payload, fieldPath); ok {
				metadata[metaKey] = value
			}
		}
		if len(metadata) > 0 {
			metaData, err := json.Marshal(metadata)
			if err != nil {
				return nil, fmt.Errorf("failed to marshal metadata: %w", err)
			}
			event.Metadata = metaData
		}
	}

	return event, nil
}

// getFieldValue extracts a value from a nested map using dot notation
func getFieldValue(payload map[string]interface{}, fieldPath string) (interface{}, bool) {
	// JSONPath expects a root object, so wrap in "$."
	expr := "$." + fieldPath
	result, err := jsonpath.Get(expr, payload)
	if err != nil || result == nil {
		return nil, false
	}

	// jsonpath.Get may return []interface{} if multiple matches
	if slice, ok := result.([]interface{}); ok && len(slice) > 0 {
		return slice[0], true
	}
	return result, true
}

// generateEventID creates a unique event identifier
func generateEventID() string {
	return fmt.Sprintf("evt_%d", time.Now().UnixNano())
}

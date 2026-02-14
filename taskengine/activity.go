package taskengine

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"time"

	"github.com/contenox/vibe/libtracker"
	libkv "github.com/contenox/vibe/libkvstore"
	"github.com/google/uuid"
)

type KVActivitySink struct {
	kvManager libkv.KVManager
}

func NewKVActivityTracker(kvManager libkv.KVManager) *KVActivitySink {
	return &KVActivitySink{
		kvManager: kvManager,
	}
}

type TrackedEvent struct {
	ID         string            `json:"id"`
	Operation  string            `json:"operation"`
	Subject    string            `json:"subject"`
	Start      time.Time         `json:"start"`
	End        *time.Time        `json:"end,omitempty"`
	Error      *string           `json:"error,omitempty"`
	EntityID   *string           `json:"entityID,omitempty"`
	EntityData any               `json:"entityData,omitempty"`
	Duration   float64           `json:"duration"` // Duration in milliseconds
	Metadata   map[string]string `json:"metadata,omitempty"`
	RequestID  string            `json:"requestID,omitempty"`
}

func (c *CapturedStateUnit) MarshalJSON() ([]byte, error) {
	type Alias CapturedStateUnit
	return json.Marshal(&struct {
		Duration float64 `json:"duration"` // Convert to milliseconds
		*Alias
	}{
		Duration: float64(c.Duration) / float64(time.Millisecond),
		Alias:    (*Alias)(c),
	})
}

func (c *CapturedStateUnit) UnmarshalJSON(data []byte) error {
	type Alias CapturedStateUnit
	aux := &struct {
		Duration float64 `json:"duration"`
		*Alias
	}{
		Alias: (*Alias)(c),
	}

	// First unmarshal into our auxiliary struct
	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}

	// Convert milliseconds back to nanoseconds for time.Duration
	c.Duration = time.Duration(aux.Duration * float64(time.Millisecond))
	return nil
}

type TrackedRequest struct {
	ID string `json:"id"`
}

func (t *KVActivitySink) Start(
	ctx context.Context,
	operation string,
	subject string,
	kvArgs ...any,
) (func(error), func(string, any), func()) {
	startTime := time.Now().UTC()
	metadata := extractMetadata(kvArgs...)

	// Initialize event with start information
	event := &TrackedEvent{
		ID:        uuid.New().String(),
		Operation: operation,
		Subject:   subject,
		Start:     startTime,
		Metadata:  metadata,
	}
	if reqID, ok := ctx.Value(libtracker.ContextKeyRequestID).(string); ok {
		event.RequestID = reqID
	}
	// Define lifecycle handlers
	reportErr := func(err error) {
		if err != nil {
			errStr := err.Error()
			event.Error = &errStr
		}
	}
	reportChange := func(id string, data any) {
		event.EntityID = &id
		event.EntityData = data
	}

	end := func() {
		now := time.Now().UTC()
		event.End = &now
		duration := time.Since(startTime)
		if duration > 0 {
			event.Duration = float64(duration) / float64(time.Millisecond)
		}
		// Prepare event for storage
		data, err := json.Marshal(event)
		if err != nil {
			log.Printf("SERVERBUG: Failed to marshal activity event: %v", err)
			return
		}

		// Store in key-value system
		kv, err := t.kvManager.Executor(ctx)
		if err != nil {
			log.Printf("SERVERBUG: Failed to get KV executor: %v", err)
			return
		}

		// Push to activity log and trim
		if err := kv.ListPush(ctx, "activity:log", data); err != nil {
			log.Printf("SERVERBUG: Failed to push activity event: %v", err)
		}

		// Maintain last 1000 events
		if err := kv.ListTrim(ctx, "activity:log", 0, 999); err != nil {
			log.Printf("SERVERBUG: Failed to trim activity log: %v", err)
		}
		if event.RequestID != "" {
			reqKey := "activity:request:" + event.RequestID
			if err := kv.ListPush(ctx, reqKey, data); err != nil {
				log.Printf("SERVERBUG: Failed to push requestID activity event: %v", err)
			}
			trackedRequest := TrackedRequest{
				ID: event.RequestID,
			}
			treq, err := json.Marshal(trackedRequest)
			if err != nil {
				log.Printf("SERVERBUG: Failed to marshal tracked request: %v", err)
			}
			if err := kv.SetAdd(ctx, "activity:requests", treq); err != nil {
				log.Printf("SERVERBUG: Failed to track requestID: %v", err)
			}
			if err := kv.SetAdd(ctx, "activity:"+event.Operation+","+event.Subject, treq); err != nil {
				log.Printf("SERVERBUG: Failed to track requestID: %v", err)
			}
			op := Operation{Operation: event.Operation, Subject: event.Subject}
			opData, err := json.Marshal(op)
			if err != nil {
				log.Printf("SERVERBUG: Failed to marshal operation: %v", err)
			} else {
				if err := kv.SetAdd(ctx, "activity:operations", opData); err != nil {
					log.Printf("SERVERBUG: Failed to track operation: %v", err)
				}
			}
		}
	}

	return reportErr, reportChange, end
}

func extractMetadata(args ...any) map[string]string {
	meta := make(map[string]string)
	for i := 0; i+1 < len(args); i += 2 {
		key, okKey := args[i].(string)
		val, okVal := args[i+1].(string)
		if okKey && okVal {
			meta[key] = val
		}
	}
	return meta
}

func (t *KVActivitySink) GetRecentRequestIDs(ctx context.Context, limit int) ([]TrackedRequest, error) {
	if limit <= 0 {
		limit = 100
	}

	kv, err := t.kvManager.Executor(ctx)
	if err != nil {
		return nil, err
	}

	rawItems, err := kv.SetMembers(ctx, "activity:requests")
	if err != nil {
		return nil, err
	}

	var requestIDs []TrackedRequest
	seen := make(map[string]struct{}, len(rawItems))

	for _, raw := range rawItems {
		var req TrackedRequest
		if err := json.Unmarshal(raw, &req); err != nil {
			return nil, fmt.Errorf("SERVERBUG: Failed to unmarshal tracked request: %v", err)
		}
		if _, exists := seen[req.ID]; !exists {
			seen[req.ID] = struct{}{}
			requestIDs = append(requestIDs, req)
		}
	}

	return requestIDs, nil
}

func (t *KVActivitySink) GetActivityLogs(ctx context.Context, limit int) ([]TrackedEvent, error) {
	kv, err := t.kvManager.Executor(ctx)
	if err != nil {
		return nil, err
	}

	if limit <= 0 {
		limit = 100
	}

	// Get list length
	listLen, err := kv.ListLength(ctx, "activity:log")
	if err != nil {
		return nil, err
	}

	// Determine range
	start := int64(0)
	stop := int64(limit - 1)
	if listLen < stop+1 {
		stop = listLen - 1
	}

	rawItems, err := kv.ListRange(ctx, "activity:log", start, stop)
	if err != nil {
		return nil, err
	}

	var results []TrackedEvent
	for _, raw := range rawItems {
		var evt TrackedEvent
		if err := json.Unmarshal(raw, &evt); err == nil {
			results = append(results, evt)
		}
	}

	return results, nil
}

type Operation struct {
	Operation string `json:"operation"`
	Subject   string `json:"subject"`
}

func (t *KVActivitySink) GetKnownOperations(ctx context.Context) ([]Operation, error) {
	kv, err := t.kvManager.Executor(ctx)
	if err != nil {
		return nil, err
	}

	rawItems, err := kv.SetMembers(ctx, "activity:operations")
	if err != nil {
		return nil, err
	}

	var results []Operation
	seen := make(map[string]struct{})

	for _, raw := range rawItems {
		var op Operation
		// First try to unmarshal as JSON
		err := json.Unmarshal(raw, &op)
		if err != nil {
			return nil, err
		}
		// Check if we've seen this operation
		key := op.Operation + ":" + op.Subject
		if _, exists := seen[key]; !exists {
			seen[key] = struct{}{}
			results = append(results, op)
		}
	}

	return results, nil
}

func (t *KVActivitySink) GetRequestIDByOperation(ctx context.Context, operation Operation) ([]TrackedRequest, error) {
	kv, err := t.kvManager.Executor(ctx)
	if err != nil {
		return nil, err
	}

	key := "activity:" + operation.Operation + "," + operation.Subject

	rawItems, err := kv.SetMembers(ctx, key)
	if err != nil {
		return nil, err
	}

	var results []TrackedRequest
	for _, raw := range rawItems {
		var req TrackedRequest
		if err := json.Unmarshal(raw, &req); err == nil {
			results = append(results, req)
		}
	}

	return results, nil
}

func (t *KVActivitySink) GetActivityLogsByRequestID(ctx context.Context, requestID string) ([]TrackedEvent, error) {
	if requestID == "" {
		return nil, nil
	}

	kv, err := t.kvManager.Executor(ctx)
	if err != nil {
		return nil, err
	}

	key := "activity:request:" + requestID

	rawItems, err := kv.ListRange(ctx, key, 0, -1)
	if err != nil {
		return nil, err
	}

	var results []TrackedEvent
	for _, raw := range rawItems {
		var evt TrackedEvent
		if err := json.Unmarshal(raw, &evt); err == nil {
			results = append(results, evt)
		}
	}

	return results, nil
}

func (t *KVActivitySink) GetExecutionStateByRequestID(ctx context.Context, requestID string) ([]CapturedStateUnit, error) {
	if requestID == "" {
		return nil, nil
	}

	kv, err := t.kvManager.Executor(ctx)
	if err != nil {
		return nil, err
	}

	key := "state:" + requestID

	rawItems, err := kv.ListRange(ctx, key, 0, -1)
	if err != nil {
		return nil, err
	}

	var results []CapturedStateUnit
	for _, raw := range rawItems {
		var unit CapturedStateUnit
		if err := json.Unmarshal(raw, &unit); err != nil {
			log.Printf("SERVERBUG: Failed to unmarshal CapturedStateUnit: %v", err)
			continue
		}
		results = append(results, unit)
	}

	return results, nil
}

func (t *KVActivitySink) GetStatefulRequests(ctx context.Context) ([]string, error) {
	kv, err := t.kvManager.Executor(ctx)
	if err != nil {
		return nil, err
	}

	key := "state:requests"
	rawItems, err := kv.SetMembers(ctx, key)
	if err != nil {
		return nil, err
	}

	var requestIDs []string
	for _, raw := range rawItems {
		requestIDs = append(requestIDs, string(raw))
	}
	return requestIDs, nil
}

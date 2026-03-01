package hooks

import (
	"context"
	"fmt"
	"time"

	"github.com/contenox/vibe/taskengine"
	"github.com/getkin/kin-openapi/openapi3"
)

// MockHookRepo is a mock implementation of the HookRepo interface.
type MockHookRepo struct {
	Calls           []HookCallRecord
	ResponseMap     map[string]HookResponse
	DefaultResponse HookResponse
	ErrorSequence   []error
	callCount       int
}

// HookCallRecord now only stores the arguments passed to the hook.
type HookCallRecord struct {
	Args  taskengine.HookCall
	Input any
}

// HookResponse is simplified to only contain the direct output and optional OpenAPI schema.
type HookResponse struct {
	Output any
	Schema *openapi3.T // Updated to match interface
}

// NewMockHookRegistry returns a new instance of MockHookRepo.
func NewMockHookRegistry() *MockHookRepo {
	return &MockHookRepo{
		ResponseMap: make(map[string]HookResponse),
		DefaultResponse: HookResponse{
			Output: "default mock response",
			Schema: nil, // explicitly nil
		},
	}
}

// Exec simulates execution of a hook call using the new simplified signature.
func (m *MockHookRepo) Exec(
	ctx context.Context,
	startingTime time.Time,
	input any,
	debug bool,
	args *taskengine.HookCall,
) (any, taskengine.DataType, error) {
	m.callCount++

	// Record call details with the new simplified struct.
	call := HookCallRecord{
		Args:  *args,
		Input: input,
	}
	m.Calls = append(m.Calls, call)

	// Determine error (if any)
	var err error
	if len(m.ErrorSequence) > 0 {
		err = m.ErrorSequence[0]
		if len(m.ErrorSequence) > 1 {
			m.ErrorSequence = m.ErrorSequence[1:]
		} else {
			m.ErrorSequence = nil
		}
	}

	// Get response from map or use default
	var resp HookResponse
	if specificResp, ok := m.ResponseMap[args.Name]; ok {
		resp = specificResp
	} else {
		resp = m.DefaultResponse
	}

	// Return the direct output and error, matching the new interface.
	return resp.Output, taskengine.DataTypeAny, err
}

// Reset clears all recorded calls and resets counters
func (m *MockHookRepo) Reset() {
	m.Calls = nil
	m.callCount = 0
	m.ErrorSequence = nil
}

// CallCount returns number of times Exec was called
func (m *MockHookRepo) CallCount() int {
	return m.callCount
}

// LastCall returns the most recent hook call
func (m *MockHookRepo) LastCall() *HookCallRecord {
	if len(m.Calls) == 0 {
		return nil
	}
	return &m.Calls[len(m.Calls)-1]
}

// WithResponse configures a response for a specific hook type using the new simplified HookResponse.
func (m *MockHookRepo) WithResponse(hookType string, response HookResponse) *MockHookRepo {
	m.ResponseMap[hookType] = response
	return m
}

// WithErrorSequence sets a sequence of errors to return
func (m *MockHookRepo) WithErrorSequence(errors ...error) *MockHookRepo {
	m.ErrorSequence = errors
	return m
}

func (m *MockHookRepo) Supports(ctx context.Context) ([]string, error) {
	supported := make([]string, 0, len(m.ResponseMap))
	for k := range m.ResponseMap {
		supported = append(supported, k)
	}
	return supported, nil
}

// GetSchemasForSupportedHooks returns OpenAPI schemas for all mocked hooks.
func (m *MockHookRepo) GetSchemasForSupportedHooks(ctx context.Context) (map[string]*openapi3.T, error) {
	schemas := make(map[string]*openapi3.T)
	for hookType, response := range m.ResponseMap {
		schemas[hookType] = response.Schema
	}
	return schemas, nil
}

func (m *MockHookRepo) GetToolsForHookByName(ctx context.Context, name string) ([]taskengine.Tool, error) {
	if _, ok := m.ResponseMap[name]; ok {
		return nil, fmt.Errorf("not implemented")
	}
	return nil, fmt.Errorf("%w: %q", taskengine.ErrHookNotFound, name)
}

// Ensure MockHookRepo correctly implements the updated HookRepo interface.
var _ taskengine.HookRepo = (*MockHookRepo)(nil)

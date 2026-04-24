package tools

import (
	"context"
	"fmt"
	"time"

	"github.com/contenox/contenox/runtime/taskengine"
	"github.com/getkin/kin-openapi/openapi3"
)

// MockToolsRepo is a mock implementation of the ToolsRepo interface.
type MockToolsRepo struct {
	Calls           []ToolsCallRecord
	ResponseMap     map[string]ToolsResponse
	DefaultResponse ToolsResponse
	ErrorSequence   []error
	callCount       int
}

// ToolsCallRecord now only stores the arguments passed to the tools.
type ToolsCallRecord struct {
	Args  taskengine.ToolsCall
	Input any
}

// ToolsResponse is simplified to only contain the direct output and optional OpenAPI schema.
type ToolsResponse struct {
	Output any
	Schema *openapi3.T // Updated to match interface
}

// NewMockToolsRegistry returns a new instance of MockToolsRepo.
func NewMockToolsRegistry() *MockToolsRepo {
	return &MockToolsRepo{
		ResponseMap: make(map[string]ToolsResponse),
		DefaultResponse: ToolsResponse{
			Output: "default mock response",
			Schema: nil, // explicitly nil
		},
	}
}

// Exec simulates execution of a tools call using the new simplified signature.
func (m *MockToolsRepo) Exec(
	ctx context.Context,
	startingTime time.Time,
	input any,
	debug bool,
	args *taskengine.ToolsCall,
) (any, taskengine.DataType, error) {
	m.callCount++

	// Record call details with the new simplified struct.
	call := ToolsCallRecord{
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
	var resp ToolsResponse
	if specificResp, ok := m.ResponseMap[args.Name]; ok {
		resp = specificResp
	} else {
		resp = m.DefaultResponse
	}

	// Return the direct output and error, matching the new interface.
	return resp.Output, taskengine.DataTypeAny, err
}

// Reset clears all recorded calls and resets counters
func (m *MockToolsRepo) Reset() {
	m.Calls = nil
	m.callCount = 0
	m.ErrorSequence = nil
}

// CallCount returns number of times Exec was called
func (m *MockToolsRepo) CallCount() int {
	return m.callCount
}

// LastCall returns the most recent tools call
func (m *MockToolsRepo) LastCall() *ToolsCallRecord {
	if len(m.Calls) == 0 {
		return nil
	}
	return &m.Calls[len(m.Calls)-1]
}

// WithResponse configures a response for a specific tools type using the new simplified ToolsResponse.
func (m *MockToolsRepo) WithResponse(toolsType string, response ToolsResponse) *MockToolsRepo {
	m.ResponseMap[toolsType] = response
	return m
}

// WithErrorSequence sets a sequence of errors to return
func (m *MockToolsRepo) WithErrorSequence(errors ...error) *MockToolsRepo {
	m.ErrorSequence = errors
	return m
}

func (m *MockToolsRepo) Supports(ctx context.Context) ([]string, error) {
	supported := make([]string, 0, len(m.ResponseMap))
	for k := range m.ResponseMap {
		supported = append(supported, k)
	}
	return supported, nil
}

// GetSchemasForSupportedTools returns OpenAPI schemas for all mocked tools.
func (m *MockToolsRepo) GetSchemasForSupportedTools(ctx context.Context) (map[string]*openapi3.T, error) {
	schemas := make(map[string]*openapi3.T)
	for toolsType, response := range m.ResponseMap {
		schemas[toolsType] = response.Schema
	}
	return schemas, nil
}

func (m *MockToolsRepo) GetToolsForToolsByName(ctx context.Context, name string) ([]taskengine.Tool, error) {
	if _, ok := m.ResponseMap[name]; ok {
		return nil, fmt.Errorf("not implemented")
	}
	return nil, fmt.Errorf("%w: %q", taskengine.ErrToolsNotFound, name)
}

// Ensure MockToolsRepo correctly implements the updated ToolsRepo interface.
var _ taskengine.ToolsRepo = (*MockToolsRepo)(nil)

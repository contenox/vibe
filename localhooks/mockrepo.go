package localhooks

import (
	"context"
	"time"

	"github.com/contenox/vibe/taskengine"
)

// MockHookRepo is a mock implementation of the HookProvider interface.
type MockHookRepo struct {
	Calls           []HookCallRecord
	ResponseMap     map[string]HookResponse
	DefaultResponse HookResponse
	ErrorSequence   []error
	callCount       int
}

type HookCallRecord struct {
	Args       taskengine.HookCall
	Input      any
	InputType  taskengine.DataType
	Transition string
}

type HookResponse struct {
	Status     int
	Output     any
	OutputType taskengine.DataType
	Transition string
}

// NewMockHookRegistry returns a new instance of MockHookProvider.
func NewMockHookRegistry() *MockHookRepo {
	return &MockHookRepo{
		ResponseMap: make(map[string]HookResponse),
		DefaultResponse: HookResponse{
			Output:     "default mock response",
			OutputType: taskengine.DataTypeString,
			Transition: "",
		},
	}
}

// Exec simulates execution of a hook call.
func (m *MockHookRepo) Exec(
	ctx context.Context,
	startingTime time.Time,
	input any,
	inputType taskengine.DataType,
	transition string,
	args *taskengine.HookCall,
) (int, any, taskengine.DataType, string, error) {
	m.callCount++

	// Record call details
	call := HookCallRecord{
		Args:       *args,
		Input:      input,
		InputType:  inputType,
		Transition: transition,
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

	return resp.Status, resp.Output, resp.OutputType, resp.Transition, err
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

// WithResponse configures a response for a specific hook type
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

var _ taskengine.HookRegistry = (*MockHookRepo)(nil)

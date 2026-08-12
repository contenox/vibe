package taskengine

import (
	"context"
	"time"
)

// MockTaskExecutor is a mock implementation of taskengine.TaskExecutor.
type MockTaskExecutor struct {
	// Single value responses
	MockOutput          any
	MockTransitionValue string
	MockError           error

	// Sequence responses
	MockOutputSequence          []any
	MockTaskTypeSequence        []DataType
	MockTransitionValueSequence []string
	ErrorSequence               []error

	// Tracking
	CalledWithTask   *TaskDefinition
	CalledWithInput  any
	CalledWithPrompt string
	callCount        int
}

// TaskExec is the mock implementation of the TaskExec method.
func (m *MockTaskExecutor) TaskExec(ctx context.Context, startingTime time.Time, tokenLimit int, chainContext *ChainContext, currentTask *TaskDefinition, input any, dataType DataType) (any, DataType, string, error) {
	m.callCount++
	m.CalledWithTask = currentTask
	m.CalledWithInput = input

	var output any
	if len(m.MockOutputSequence) > 0 {
		output = m.MockOutputSequence[0]
		if len(m.MockOutputSequence) > 1 {
			m.MockOutputSequence = m.MockOutputSequence[1:]
		}
	} else {
		output = m.MockOutput
	}

	var err error
	if len(m.ErrorSequence) > 0 {
		err = m.ErrorSequence[0]
		if len(m.ErrorSequence) > 1 {
			m.ErrorSequence = m.ErrorSequence[1:]
		}
	} else {
		err = m.MockError
	}

	var outputDataType DataType
	if len(m.MockTaskTypeSequence) > 0 {
		outputDataType = m.MockTaskTypeSequence[0]
		if len(m.MockTaskTypeSequence) > 1 {
			m.MockTaskTypeSequence = m.MockTaskTypeSequence[1:]
		}
	} else {
		switch v := output.(type) {
		case string:
			outputDataType = DataTypeString
		case int:
			outputDataType = DataTypeInt
		case ChatHistory:
			outputDataType = DataTypeChatHistory
		case map[string]any:
			outputDataType = DataTypeJSON
		default:
			if v == nil {
				outputDataType = dataType
			} else {
				outputDataType = DataTypeAny
			}
		}
	}

	var transitionResponse string
	if len(m.MockTransitionValueSequence) > 0 {
		transitionResponse = m.MockTransitionValueSequence[0]
		if len(m.MockTransitionValueSequence) > 1 {
			m.MockTransitionValueSequence = m.MockTransitionValueSequence[1:]
		}
	} else {
		transitionResponse = m.MockTransitionValue
	}

	if transitionResponse == "" {
		if s, ok := output.(string); ok {
			transitionResponse = s
		}
	}

	return output, outputDataType, transitionResponse, err
}

// Reset clears all mock state between tests
func (m *MockTaskExecutor) Reset() {
	m.MockOutput = nil
	m.MockTransitionValue = ""
	m.MockError = nil
	m.MockOutputSequence = nil
	m.MockTaskTypeSequence = nil
	m.MockTransitionValueSequence = nil
	m.ErrorSequence = nil
	m.CalledWithTask = nil
	m.CalledWithInput = nil
	m.CalledWithPrompt = ""
	m.callCount = 0
}

// CallCount returns how many times TaskExec was called
func (m *MockTaskExecutor) CallCount() int {
	return m.callCount
}

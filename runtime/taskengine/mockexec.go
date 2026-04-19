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

	// Get output from sequence or single value
	var output any
	if len(m.MockOutputSequence) > 0 {
		output = m.MockOutputSequence[0]
		if len(m.MockOutputSequence) > 1 {
			m.MockOutputSequence = m.MockOutputSequence[1:]
		}
	} else {
		output = m.MockOutput
	}

	// Get error from sequence or single value
	var err error
	if len(m.ErrorSequence) > 0 {
		err = m.ErrorSequence[0]
		if len(m.ErrorSequence) > 1 {
			m.ErrorSequence = m.ErrorSequence[1:]
		}
	} else {
		err = m.MockError
	}

	// Get output data type from sequence or determine from output
	var outputDataType DataType
	if len(m.MockTaskTypeSequence) > 0 {
		outputDataType = m.MockTaskTypeSequence[0]
		if len(m.MockTaskTypeSequence) > 1 {
			m.MockTaskTypeSequence = m.MockTaskTypeSequence[1:]
		}
	} else {
		// Fallback: Determine data type from output value
		switch v := output.(type) {
		case string:
			outputDataType = DataTypeString
		case bool:
			outputDataType = DataTypeBool
		case int:
			outputDataType = DataTypeInt
		case float64:
			outputDataType = DataTypeFloat
		case ChatHistory:
			outputDataType = DataTypeChatHistory
		case OpenAIChatRequest:
			outputDataType = DataTypeOpenAIChat
		case OpenAIChatResponse:
			outputDataType = DataTypeOpenAIChatResponse
		case map[string]any:
			outputDataType = DataTypeJSON
		default:
			if v == nil {
				// If output is nil, preserve input type
				outputDataType = dataType
			} else {
				outputDataType = DataTypeAny
			}
		}
	}

	// Get raw transition response from sequence or single value
	var transitionResponse string
	if len(m.MockTransitionValueSequence) > 0 {
		transitionResponse = m.MockTransitionValueSequence[0]
		if len(m.MockTransitionValueSequence) > 1 {
			m.MockTransitionValueSequence = m.MockTransitionValueSequence[1:]
		}
	} else {
		transitionResponse = m.MockTransitionValue
	}

	// If no explicit transition response was provided, infer it.
	// This is crucial for conditional handlers used in tests.
	if transitionResponse == "" {
		switch currentTask.Handler {
		case HandlePromptToCondition:
			// For condition key handler, use the string output as the transition eval.
			if s, ok := output.(string); ok {
				transitionResponse = s
			}
		default:
			// Generic fallback: if the output is a string, use it.
			if s, ok := output.(string); ok {
				transitionResponse = s
			}
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

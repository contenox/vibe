package taskengine

import "strings"

// HandlerOutputMode names how a handler's success-output type derives from its
// input type.
type HandlerOutputMode int

const (
	// HandlerOutputFixed: the handler always produces HandlerSignature.Output
	// on success, regardless of input type.
	HandlerOutputFixed HandlerOutputMode = iota
	// HandlerOutputPassthrough: the handler returns its input unchanged —
	// output type equals input type (noop, and route, whose product is the
	// transition label, not the data).
	HandlerOutputPassthrough
	// HandlerOutputDynamic: the output type is decided at runtime by the tool that
	// executes (the tools handler); the linter treats it as DataTypeAny unless the
	// task's OutputTemplate forces a rendered string.
	HandlerOutputDynamic
	// HandlerOutputNone: the handler never succeeds (raise_error); only its
	// on_failure edge can ever be taken, success branches are dead.
	HandlerOutputNone
)

// HandlerSignature is the frozen I/O contract of one task handler.
type HandlerSignature struct {
	// Inputs is the closed set of DataTypes the handler accepts, in the order teaching
	// errors name them; empty means every DataType is accepted.
	Inputs []DataType
	// Mode says how the success-output type derives from the input type.
	Mode HandlerOutputMode
	// Output is the produced type when Mode == HandlerOutputFixed.
	Output DataType
	// SuccessEvals is the closed transition-eval vocabulary the handler can emit on
	// success — the only values a TransitionBranch can match; nil means the vocabulary
	// is open and the linter cannot prove a branch dead.
	SuccessEvals []string
}

// AcceptsInput reports whether the handler's closed input set admits dt; DataTypeAny
// is never "admitted" here, callers must special-case it.
func (s HandlerSignature) AcceptsInput(dt DataType) bool {
	if len(s.Inputs) == 0 {
		return true
	}
	for _, in := range s.Inputs {
		if in == dt {
			return true
		}
	}
	return false
}

func (s HandlerSignature) acceptsDescription() string {
	if len(s.Inputs) == 0 {
		return "any input type"
	}
	names := make([]string, len(s.Inputs))
	for i, in := range s.Inputs {
		d := in
		names[i] = d.String()
	}
	return strings.Join(names, ", ")
}

var handlerSignatures = map[TaskHandler]HandlerSignature{
	HandleChatCompletion: {
		Inputs:       []DataType{DataTypeString, DataTypeChatHistory},
		Mode:         HandlerOutputFixed,
		Output:       DataTypeChatHistory,
		SuccessEvals: []string{TransitionToolCall, TransitionExecuted},
	},
	HandleExecuteToolCalls: {
		Inputs:       []DataType{DataTypeChatHistory},
		Mode:         HandlerOutputFixed,
		Output:       DataTypeChatHistory,
		SuccessEvals: []string{TransitionNoop, TransitionNoCallsFound, TransitionToolsExecuted, TransitionFailed},
	},
	HandleRoute: {
		Inputs: []DataType{DataTypeString, DataTypeInt, DataTypeChatHistory},
		Mode:   HandlerOutputPassthrough,
	},
	HandleTools: {
		Mode: HandlerOutputDynamic,
	},
	HandleNoop: {
		Mode:         HandlerOutputPassthrough,
		SuccessEvals: []string{TransitionNoop},
	},
	HandleRaiseError: {
		Inputs: []DataType{DataTypeString, DataTypeInt, DataTypeChatHistory},
		Mode:   HandlerOutputNone,
	},
}

// HandlerSignatureFor returns the frozen contract for h; the bool is false for a
// handler the table does not know, which validateChain already rejects.
func HandlerSignatureFor(h TaskHandler) (HandlerSignature, bool) {
	sig, ok := handlerSignatures[h]
	return sig, ok
}

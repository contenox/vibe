package taskengine

import "strings"

// This file is the closed handler I/O contract table: it freezes as data
// the input/output judgement taskexec.go's handler switch makes implicitly,
// so chainlint.go can reject an impossible edge before anything runs.
// Every entry must match what the corresponding TaskExec case actually
// does; handler_signatures_test.go pins the behavioral half.

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
	// HandlerOutputDynamic: the output type is decided at runtime by the tool
	// that executes (the tools handler). The linter must treat it as
	// DataTypeAny — unless the task's OutputTemplate forces a rendered string.
	HandlerOutputDynamic
	// HandlerOutputNone: the handler never succeeds (raise_error). Only its
	// on_failure edge can ever be taken; success branches are dead.
	HandlerOutputNone
)

// HandlerSignature is the frozen I/O contract of one task handler.
type HandlerSignature struct {
	// Inputs is the closed set of DataTypes the handler accepts, in the order
	// teaching errors should name them. Empty means the handler accepts every
	// DataType.
	Inputs []DataType
	// Mode says how the success-output type derives from the input type.
	Mode HandlerOutputMode
	// Output is the produced type when Mode == HandlerOutputFixed.
	Output DataType
	// SuccessEvals is the closed transition-eval vocabulary the handler can
	// emit on success — the only values a TransitionBranch can ever match for
	// this handler. Nil means the vocabulary is open (route emits the model's
	// chosen label; a tools task's OutputTemplate replaces the eval with its
	// rendered text) and the linter cannot prove a branch dead.
	SuccessEvals []string
}

// AcceptsInput reports whether the handler's closed input set admits dt.
// DataTypeAny is never "admitted" here — an Any-typed value is unknown at load
// time and stays the runtime backstop's problem; callers must special-case it.
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

// acceptsDescription renders the accept set for teaching errors:
// "string, chat_history", or "any input type" for an open set.
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

// handlerSignatures is the frozen contract table. Each row documents where
// in taskexec.go the contract is implemented, so a change there knows what
// to update here.
var handlerSignatures = map[TaskHandler]HandlerSignature{
	// chat_completion coerces a string into a single-user-message history,
	// otherwise requires ChatHistory. Always yields the updated ChatHistory,
	// evaluating to tool_call or executed.
	HandleChatCompletion: {
		Inputs:       []DataType{DataTypeString, DataTypeChatHistory},
		Mode:         HandlerOutputFixed,
		Output:       DataTypeChatHistory,
		SuccessEvals: []string{TransitionToolCall, TransitionExecuted},
	},
	// execute_tool_calls only understands a ChatHistory whose last message
	// may carry tool calls; it appends tool results and returns the
	// history. TransitionFailed is listed even though on_failure routes a
	// failing task first, so a defensive equals:"failed" branch is
	// dead-but-harmless rather than an authoring error.
	HandleExecuteToolCalls: {
		Inputs:       []DataType{DataTypeChatHistory},
		Mode:         HandlerOutputFixed,
		Output:       DataTypeChatHistory,
		SuccessEvals: []string{TransitionNoop, TransitionNoCallsFound, TransitionToolsExecuted, TransitionFailed},
	},
	// route classifies via getPrompt and returns the input unchanged; its
	// product is the transition label. SuccessEvals stays open since the
	// vocabulary is the task's own equals branches (the linter separately
	// requires at least one).
	HandleRoute: {
		Inputs: []DataType{DataTypeString, DataTypeInt, DataTypeChatHistory},
		Mode:   HandlerOutputPassthrough,
	},
	// tools passes the input to ToolsRepo.Exec verbatim; the output type is
	// whatever the tool returned, unknowable at load time. An OutputTemplate
	// rewrites both output and eval to its rendered text.
	HandleTools: {
		Mode: HandlerOutputDynamic,
	},
	// noop returns its input before any type inspection: a true identity.
	HandleNoop: {
		Mode:         HandlerOutputPassthrough,
		SuccessEvals: []string{TransitionNoop},
	},
	// raise_error turns its input into an error message via getPrompt
	// (string, int, chat_history); the accept set is closed so any other
	// type reports getPrompt's complaint, not the author's message. Never succeeds.
	HandleRaiseError: {
		Inputs: []DataType{DataTypeString, DataTypeInt, DataTypeChatHistory},
		Mode:   HandlerOutputNone,
	},
}

// HandlerSignatureFor returns the frozen contract for h. The bool is false for
// a handler the table does not know — which validateChain already rejects.
func HandlerSignatureFor(h TaskHandler) (HandlerSignature, bool) {
	sig, ok := handlerSignatures[h]
	return sig, ok
}

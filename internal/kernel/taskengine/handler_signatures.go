package taskengine

import "strings"

// This file is the CLOSED handler I/O contract table. taskexec.go's handler
// switch implements these contracts implicitly — each case decides at runtime
// which input DataTypes it tolerates and what it hands back. This table freezes
// that judgement as data so the load-time chain linter (chainlint.go) can walk a
// chain's dataflow BEFORE anything runs and reject an impossible edge with a
// teaching error, instead of the runtime discovering it mid-run as a SEVERBUG.
//
// INVARIANT: every entry here must match what the corresponding case in
// SimpleExec.TaskExec actually does. A handler added to tasktype.go without a
// row here fails LintChain for every chain that uses it (unknown handler), so
// the table cannot silently fall behind the vocabulary. The behavioral half of
// the invariant is pinned by handler_signatures_test.go, which drives the real
// executor against each contract.

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

// handlerSignatures is THE table. Each row documents where in taskexec.go the
// contract is implemented, so a change there knows what to update here.
var handlerSignatures = map[TaskHandler]HandlerSignature{
	// chat_completion coerces a string into a single-user-message history and
	// otherwise requires ChatHistory; every other type is rejected with
	// "requires input of type 'chat_history' or 'string'". It always yields the
	// updated ChatHistory and evaluates to tool_call (model requested tools) or
	// executed (finished turn).
	HandleChatCompletion: {
		Inputs:       []DataType{DataTypeString, DataTypeChatHistory},
		Mode:         HandlerOutputFixed,
		Output:       DataTypeChatHistory,
		SuccessEvals: []string{TransitionToolCall, TransitionExecuted},
	},
	// execute_tool_calls only understands a ChatHistory whose last message may
	// carry tool calls; it appends tool results and returns the history.
	// TransitionFailed is listed although the engine routes a failing task via
	// on_failure before branches are evaluated — a defensive equals:"failed"
	// branch is dead-but-harmless config, not an authoring error worth failing.
	HandleExecuteToolCalls: {
		Inputs:       []DataType{DataTypeChatHistory},
		Mode:         HandlerOutputFixed,
		Output:       DataTypeChatHistory,
		SuccessEvals: []string{TransitionNoop, TransitionNoCallsFound, TransitionToolsExecuted, TransitionFailed},
	},
	// route classifies via getPrompt (string, int) or a rendered history
	// (chat_history) and returns THE INPUT unchanged — its product is the
	// transition label. The label vocabulary is the task's own equals branches,
	// so SuccessEvals stays open here; the linter separately requires that a
	// route task declares at least one equals branch.
	HandleRoute: {
		Inputs: []DataType{DataTypeString, DataTypeInt, DataTypeChatHistory},
		Mode:   HandlerOutputPassthrough,
	},
	// tools passes the input to ToolsRepo.Exec verbatim, so any type goes in;
	// what comes out is whatever the tool returned (normalized), unknowable at
	// load time. An OutputTemplate rewrites both the output (to a rendered
	// string) and the eval (to that same text), so the vocabulary is open.
	HandleTools: {
		Mode: HandlerOutputDynamic,
	},
	// noop returns its input before any type inspection: a true identity.
	HandleNoop: {
		Mode:         HandlerOutputPassthrough,
		SuccessEvals: []string{TransitionNoop},
	},
	// raise_error turns its input into an error message via getPrompt, which
	// reads string, int, and chat_history. Any other type still errors, but
	// with getPrompt's complaint instead of the author's message — which is why
	// the accept set is closed. It never succeeds.
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

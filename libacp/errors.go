package libacp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

const (
	ErrParseError     = -32700
	ErrInvalidRequest = -32600
	ErrMethodNotFound = -32601
	ErrInvalidParams  = -32602
	ErrInternalError  = -32603

	ErrAuthRequired = -32000
	// ErrRequestTimeout is the wire signal that a peer's handler ran out of
	// time, since a Go sentinel cannot cross the JSON-RPC boundary. Matches
	// the code MCP implementations use for the same condition.
	ErrRequestTimeout   = -32001
	ErrResourceNotFound = -32002
)

// Error is a JSON-RPC error object. The exported fields are the entire wire
// contract; cause is process-local and never serialized.
type Error struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`

	// cause is the handler error this Error was built from, so errors.Is/As
	// still work while the value has not left the process.
	cause error
}

// Unwrap exposes the originating handler error; an Error decoded from the
// wire has no cause and returns nil. Classify a remote failure via Code
// instead (see IsTimeoutError).
func (e *Error) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.cause
}

func (e *Error) Error() string {
	if e == nil {
		return "<nil libacp.Error>"
	}
	return fmt.Sprintf("libacp: rpc error %d: %s", e.Code, e.Message)
}

func NewError(code int, message string) *Error {
	return &Error{Code: code, Message: message}
}

func NewErrorf(code int, format string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...)}
}

func ParseError(msg string) *Error     { return NewError(ErrParseError, msg) }
func InvalidRequest(msg string) *Error { return NewError(ErrInvalidRequest, msg) }
func MethodNotFound(method string) *Error {
	return NewError(ErrMethodNotFound, "method not found: "+method)
}
func InvalidParams(msg string) *Error { return NewError(ErrInvalidParams, msg) }
func InternalError(msg string) *Error { return NewError(ErrInternalError, msg) }

// AsError converts a handler error into the JSON-RPC error that goes on the
// wire. It retains err as cause for same-process sentinel matching, and
// promotes a deadline to ErrRequestTimeout so a remote caller can tell
// "too slow, retry" from "broken, give up". Everything else becomes
// ErrInternalError.
func AsError(err error) *Error {
	if err == nil {
		return nil
	}
	if e, ok := err.(*Error); ok {
		return e
	}
	code := ErrInternalError
	if errors.Is(err, context.DeadlineExceeded) {
		code = ErrRequestTimeout
	}
	return &Error{Code: code, Message: err.Error(), cause: err}
}

// HandlerDrainTimeout bounds how long Run waits, after shutdown cancels
// everything, for in-flight handler goroutines to return. A backstop for a
// handler that ignores its cancelled context; should never fire normally.
const HandlerDrainTimeout = 10 * time.Second

// ErrHandlerDrainTimeout reports that Run gave up waiting for handler
// goroutines to return (see HandlerDrainTimeout). Some handler may still be
// running, so the caller's teardown of shared state is unsafe.
var ErrHandlerDrainTimeout = errors.New("libacp: timed out waiting for handler goroutines to return")

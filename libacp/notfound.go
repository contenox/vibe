package libacp

import (
	"errors"
	"fmt"
	"os"
	"strings"
)

// IsNotFound reports whether err is a peer's answer of "that resource does
// not exist" (file/resource sense, not lifecycle): only a typed *Error with
// Code == ErrResourceNotFound, or a subject-describing code whose message
// says "not found", counts — a raw error's text and protocol-level codes are
// never classified here.
func IsNotFound(err error) bool {
	if err == nil {
		return false
	}
	var e *Error
	if !errors.As(err, &e) || e == nil {
		return false
	}
	if e.Code == ErrResourceNotFound {
		return true
	}
	switch e.Code {
	case ErrParseError, ErrInvalidRequest, ErrMethodNotFound, ErrInvalidParams, ErrAuthRequired, ErrRequestTimeout:
		return false
	}
	return strings.Contains(strings.ToLower(e.Message), "not found")
}

// AsNotExist normalizes a not-found failure (per IsNotFound) into an error
// satisfying errors.Is(err, os.ErrNotExist); any other error, including nil,
// is returned unchanged.
func AsNotExist(err error) error {
	if err == nil {
		return nil
	}
	if IsNotFound(err) {
		return fmt.Errorf("%w: %v", os.ErrNotExist, err)
	}
	return err
}

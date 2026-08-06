package libacp

import (
	"errors"
	"fmt"
	"os"
	"strings"
)

// IsNotFound reports whether err is a peer's answer of "that resource does
// not exist" (file/resource sense, not lifecycle). Only a typed *Error
// counts — a raw error's text is never classified here, so a startup failure
// like exec.ErrNotFound can't be misread as a missing file.
//
// Code == ErrResourceNotFound is the canonical signal. As a fallback, some
// agents answer fs/read_text_file with a generic ErrInternalError whose
// message just says "not found", so the message is also checked — but only
// for codes describing the request's subject. Protocol-level codes (parse,
// invalid request/params, method not found, auth required) and
// ErrRequestTimeout describe the request itself and are excluded, since
// message-sniffing them would misclassify an unimplemented method or a
// timeout as a missing file.
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
// satisfying errors.Is(err, os.ErrNotExist), so fs/* callers can branch with
// the same predicate as local I/O. Any other error, including nil, is
// returned unchanged.
func AsNotExist(err error) error {
	if err == nil {
		return nil
	}
	if IsNotFound(err) {
		return fmt.Errorf("%w: %v", os.ErrNotExist, err)
	}
	return err
}

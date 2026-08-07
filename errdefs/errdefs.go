package errdefs

import (
	"errors"
	"fmt"
)

var (
	ErrBadRequest          = errors.New("bad request")
	ErrEmptyRequest        = errors.New("empty request")
	ErrEmptyRequestBody    = errors.New("empty request body")
	ErrImmutableModel      = errors.New("immutable model")
	ErrUnprocessableEntity = errors.New("unprocessable entity")
	ErrNotFound            = errors.New("not found")
	ErrConflict            = errors.New("conflict")
	ErrMissingParameter    = errors.New("missing parameter")
	ErrInvalidParameter    = errors.New("invalid parameter value")
)

func BadRequest(message string) error {
	if message == "" {
		return ErrBadRequest
	}
	return fmt.Errorf("%w: %s", ErrBadRequest, message)
}

func NotFound(message string) error {
	if message == "" {
		return ErrNotFound
	}
	return fmt.Errorf("%w: %s", ErrNotFound, message)
}

func Conflict(message string) error {
	if message == "" {
		return ErrConflict
	}
	return fmt.Errorf("%w: %s", ErrConflict, message)
}

func MissingParameter(param, message string) error {
	if message == "" {
		return fmt.Errorf("%w: %s", ErrMissingParameter, param)
	}
	return fmt.Errorf("%w: %s: %s", ErrMissingParameter, param, message)
}

func InvalidParameterValue(param, message string) error {
	if message == "" {
		return fmt.Errorf("%w: %s", ErrInvalidParameter, param)
	}
	return fmt.Errorf("%w: %s: %s", ErrInvalidParameter, param, message)
}

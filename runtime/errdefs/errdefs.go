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
)

func BadRequest(message string) error {
	if message == "" {
		return ErrBadRequest
	}
	return fmt.Errorf("%w: %s", ErrBadRequest, message)
}

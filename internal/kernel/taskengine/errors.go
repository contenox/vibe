package taskengine

import "errors"

// ErrContextLengthExceeded is returned when the input or chat history exceeds the allowed context length.
var ErrContextLengthExceeded = errors.New("exceeds context length")

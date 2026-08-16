package librelay

import (
	"errors"
	"fmt"
	"math/rand/v2"
)

// MaxTraceBytes bounds [Frame.Trace]. It is far below MaxIDBytes because a trace
// is not addressable and is only ever spent on a log field.
const MaxTraceBytes = 128

// TraceAlphabet is the complete set of bytes [Frame.Trace] may contain:
// unreserved URL characters and nothing else.
const TraceAlphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"

// Trace validation failures: the value is unsafe to log.
var (
	ErrTraceTooLong = errors.New("librelay: trace exceeds MaxTraceBytes")
	ErrTraceCharset = errors.New("librelay: trace contains a byte outside TraceAlphabet")
)

// ValidTraceID reports whether s may be carried in [Frame.Trace]. The empty
// string is valid.
func ValidTraceID(s string) bool {
	return len(s) <= MaxTraceBytes && badTraceByte(s) < 0
}

// badTraceByte returns the index of the first byte outside [TraceAlphabet], or
// -1. The ranges below are [TraceAlphabet] spelled a second way; the two are
// held to the same answer by TestUnit_TraceAlphabetMatchesTheScanner.
func badTraceByte(s string) int {
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= '0' && c <= '9', c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c == '-', c == '_':
		default:
			return i
		}
	}
	return -1
}

// NewTraceID mints a fresh trace for one action — one browser request, one
// prompt — never once per connection. It is a correlation key only: do not reuse
// one as a token, a nonce or an idempotency key.
func NewTraceID() string {
	return fmt.Sprintf("tr-%016x", rand.Uint64())
}

// validateTrace reports why trace may not be carried, or nil. Neither error
// quotes the value, only its length or the offending position.
func validateTrace(trace string) error {
	if len(trace) > MaxTraceBytes {
		return fmt.Errorf("%w: %d bytes", ErrTraceTooLong, len(trace))
	}
	if i := badTraceByte(trace); i >= 0 {
		return fmt.Errorf("%w: at byte %d", ErrTraceCharset, i)
	}
	return nil
}

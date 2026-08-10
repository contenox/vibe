package librelay

import (
	"errors"
	"fmt"
	"math/rand/v2"
)

// MaxTraceBytes bounds [Frame.Trace]. It is far below MaxIDBytes because a
// trace is not addressable: nothing routes on it, nothing looks it up, and the
// only thing it is ever spent on is a log field. A ceiling this low is what
// stops a peer from renting space in every activity record this runtime writes.
const MaxTraceBytes = 128

// TraceAlphabet is the complete set of bytes [Frame.Trace] may contain:
// unreserved URL characters and nothing else.
//
// It is deliberately narrower than the rule the routing identifiers obey. Those
// only exclude what corrupts a log line ([ErrControlChar]) because they carry a
// peer's chosen names; a trace carries no name and needs no expressiveness, so
// it is restricted to the alphabet [NewTraceID] emits. That leaves no quote, no
// separator, no whitespace and no non-ASCII byte — nothing that could be read as
// structure by whatever the value is eventually pasted into.
const TraceAlphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"

// Trace validation failures. Unlike the errors in frame.go these do not mean
// "unroutable"; they mean "unsafe to log", which [Frame.Validate] rejects for
// the same reason and on the same terms.
var (
	ErrTraceTooLong = errors.New("librelay: trace exceeds MaxTraceBytes")
	ErrTraceCharset = errors.New("librelay: trace contains a byte outside TraceAlphabet")
)

// ValidTraceID reports whether s may be carried in [Frame.Trace]. The empty
// string is valid: absent is the normal state of an untraced frame, not a
// defect.
//
// It is exported so a receiver can check a value it did not get from
// [Reader.ReadFrame] — a frame built in-process, or one that crossed a hop that
// predates this field — before letting it reach a log. Validating twice costs a
// scan of at most MaxTraceBytes and removes the need to reason about which
// frames arrived by which path.
func ValidTraceID(s string) bool {
	return len(s) <= MaxTraceBytes && badTraceByte(s) < 0
}

// badTraceByte returns the index of the first byte outside [TraceAlphabet], or
// -1. It reports the position rather than the byte so a caller can say what was
// wrong without quoting the value: the whole reason the alphabet is bounded is
// that this string must not reach a log, and an error message is a log line.
//
// The ranges below are the alphabet spelled a second way, for a scan that does
// not search a 64-byte string per input byte. TestUnit_TraceAlphabetMatchesTheScanner
// holds the two spellings to the same answer for all 256 bytes, so widening one
// without the other cannot ship.
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

// NewTraceID mints a fresh trace for one action. Call it where an action begins
// — one browser request, one prompt — and never once per connection: a value
// that lives as long as a socket files a day's work under one key and
// correlates nothing.
//
// Uses math/rand/v2, not crypto/rand, deliberately and for the same reason
// libtracker.WithNewRequestID does: a trace is a correlation key only, never
// authenticated and never authorized on, so the only requirement is collision
// avoidance. Do not reuse one as a token, a nonce or an idempotency key.
//
// The "tr-" prefix makes the value self-describing in a log where request ids
// and span ids sit beside it.
func NewTraceID() string {
	return fmt.Sprintf("tr-%016x", rand.Uint64())
}

// validateTrace reports why trace may not be carried, or nil. Length is checked
// before the alphabet so an oversized value reports its size rather than the
// first byte that happened to be unacceptable.
//
// Neither error quotes the value, only its length or the offending position.
// The value was just judged unfit for a log field and an error message is a log
// field, so echoing it would be the one place the bound does not hold.
func validateTrace(trace string) error {
	if len(trace) > MaxTraceBytes {
		return fmt.Errorf("%w: %d bytes", ErrTraceTooLong, len(trace))
	}
	if i := badTraceByte(trace); i >= 0 {
		return fmt.Errorf("%w: at byte %d", ErrTraceCharset, i)
	}
	return nil
}

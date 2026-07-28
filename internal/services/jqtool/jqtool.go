// Package jqtool is the agent's jq engine: one tool, `jq_query`, that runs a
// jq program (github.com/itchyny/gojq, pure Go) over a JSON or YAML document
// and returns the emitted values. It is allow-tier: read-only, no network
// access (input/inputs/import/include/$ENV are all compiled out), and every
// execution is deadline-bounded including recursion.
package jqtool

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// ToolsProviderName is the tools-provider key this package registers under.
// Policy addressing is `tools: "jq"`.
const ToolsProviderName = "jq"

// ToolQuery is the single tool this provider exposes.
const ToolQuery = "jq_query"

// toolNames is the declaration order used by Supports and the tool list.
var toolNames = []string{ToolQuery}

// Limits. Each is asserted by a test; the exported ones are the numbers a
// caller has reason to know.
const (
	// DefaultDeadline bounds one jq_query execution, including recursion: a
	// non-terminating or exponential filter is cut off here rather than hanging.
	DefaultDeadline = 2 * time.Second

	// MaxDeadline ceilings a per-call `deadline_ms` override.
	MaxDeadline = 30 * time.Second

	// MaxInputBytes caps the input document (the file behind `path`, or the
	// inline `input` string).
	MaxInputBytes = 8 << 20

	// MaxOutputBytes caps the emitted values in one result. A cut is never
	// silent: Result.Truncated and Result.Note say what was withheld.
	MaxOutputBytes = 32 << 10
)

const (
	// defaultMaxResults is how many emitted values a call returns when `max`
	// is not given.
	defaultMaxResults = 200

	// maxResultsCeiling ceilings a `max` override.
	maxResultsCeiling = 5000

	// maxFilterBytes caps the jq program itself, refused before parsing.
	maxFilterBytes = 4 << 10

	// maxInputDocs caps how many documents one input may carry. JSON is read
	// as a stream (concatenated / newline-delimited values) and YAML as a
	// multi-document stream ("---"); the filter runs once per document.
	maxInputDocs = 1000

	// maxValueDepth bounds nesting when a decoded document is normalized, so a
	// deeply nested document is a refusal rather than a stack overflow.
	maxValueDepth = 200
)

// Errors carry a "jq: " prefix and a severity marker. Every failure is
// recoverable by a corrected call except ErrNoWorkspaceRoot.
const (
	severityRecoverable = "(recoverable: adjust parameters and retry)"
	severityFatalToken  = "(fatal:"
)

// Sentinel errors, for errors.Is by callers that need to branch.
var (
	// ErrEscapesWorkspace means a `path` resolved outside the allowed
	// directory (traversal, absolute path, or an escaping symlink).
	ErrEscapesWorkspace = errors.New("jq: path escapes allowed directory")

	// ErrNoWorkspaceRoot means no allowed directory is configured, so no
	// `path` can be resolved. Inline `input` still works.
	ErrNoWorkspaceRoot = errors.New("jq: no workspace root")
)

// maxEchoRunes bounds how much of a model-supplied string an error echoes
// back, since every argument here is model-controlled.
const maxEchoRunes = 120

// echoArg renders a model-supplied argument for an error message: clamped,
// then Go-quoted so control characters, NULs and bidi overrides are escaped.
func echoArg(s string) string {
	r := []rune(s)
	if len(r) > maxEchoRunes {
		return strconv.Quote(string(r[:maxEchoRunes])) + fmt.Sprintf("… (+%d more characters)", len(r)-maxEchoRunes)
	}
	return strconv.Quote(s)
}

// echoErr renders a wrapped lower-level error inside a teaching message:
// clamped and sanitized, since the wrapped text often embeds the argument
// that failed. Non-printable runes become '?'; newlines are kept because
// gojq's multi-line parse errors (with their caret line) are worth keeping.
func echoErr(err error) string {
	if err == nil {
		return ""
	}
	var b strings.Builder
	for i, r := range []rune(err.Error()) {
		switch {
		case i >= maxEchoRunes:
			b.WriteString("…")
			return b.String()
		case r == '\n' || r == '\t' || unicode.IsPrint(r):
			b.WriteRune(r)
		default:
			b.WriteRune('?')
		}
	}
	return b.String()
}

// echoName renders a model-supplied argument name for an error message: same
// clamp as echoArg, but unquoted (bare comma-separated list voice).
func echoName(s string) string {
	var b strings.Builder
	for i, r := range []rune(s) {
		switch {
		case i >= maxEchoRunes:
			b.WriteString("…")
			return b.String()
		case unicode.IsPrint(r):
			b.WriteRune(r)
		default:
			b.WriteRune('?')
		}
	}
	return b.String()
}

// recoverablef builds a teaching error tagged recoverable-by-correction.
func recoverablef(format string, a ...any) error {
	return errors.New(fmt.Sprintf(format, a...) + " " + severityRecoverable)
}

// wrapRecoverable tags a sentinel-wrapping error recoverable unless it
// already carries a severity marker.
func wrapRecoverable(sentinel error, format string, a ...any) error {
	msg := fmt.Sprintf(format, a...)
	if hasSeverityMarker(msg) {
		return fmt.Errorf("%w: %s", sentinel, msg)
	}
	return fmt.Errorf("%w: %s %s", sentinel, msg, severityRecoverable)
}

func hasSeverityMarker(s string) bool {
	return strings.Contains(s, severityRecoverable) || strings.Contains(s, severityFatalToken)
}

// Result is the tool's payload, returned as DataTypeJSON.
type Result struct {
	// Filter echoes the jq program, clamped.
	Filter string `json:"filter"`
	// Source names what was queried: the workspace-relative path, or the
	// inline marker. Never the host-absolute path.
	Source string `json:"source"`
	// Format is the parser actually used ("json" or "yaml"); it may have been
	// sniffed rather than declared.
	Format string `json:"format"`
	// Documents is how many documents the input carried: 1 for an ordinary
	// file, more for a JSON Lines or multi-document YAML stream.
	Documents int `json:"documents"`
	// Values are the emitted values, compact JSON, in emission order.
	Values []json.RawMessage `json:"values"`
	// Count is len(Values).
	Count int `json:"count"`
	// Truncated marks a result a cap stopped; never set without Note.
	Truncated bool   `json:"truncated,omitempty"`
	Note      string `json:"note,omitempty"`
}

// appendNote joins result notes with a single space.
func appendNote(existing, add string) string {
	switch {
	case add == "":
		return existing
	case existing == "":
		return add
	default:
		return existing + " " + add
	}
}

const (
	maxInt = int(^uint(0) >> 1)
	minInt = -maxInt - 1
)

// intFromFloat converts a JSON number to an int without Go's undefined
// float→int behavior outside the integer range: out-of-range saturates, NaN
// reports false (caller takes the documented default).
func intFromFloat(f float64) (int, bool) {
	switch {
	case f != f: // NaN
		return 0, false
	case f >= float64(maxInt):
		return maxInt, true
	case f <= float64(minInt):
		return minInt, true
	}
	return int(f), true
}

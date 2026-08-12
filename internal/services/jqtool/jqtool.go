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

// ToolsProviderName is the tools-provider key this package registers under; policy addresses it as `tools: "jq"`.
const ToolsProviderName = "jq"

// ToolQuery is the single tool this provider exposes.
const ToolQuery = "jq_query"

var toolNames = []string{ToolQuery}

// Limits on one jq_query execution.
const (
	// DefaultDeadline bounds one jq_query execution, including recursion: a
	// non-terminating or exponential filter is cut off here rather than hanging.
	DefaultDeadline = 2 * time.Second

	// MaxDeadline ceilings a per-call `deadline_ms` override.
	MaxDeadline = 30 * time.Second

	// MaxInputBytes caps the input document (the file behind `path`, or the
	// inline `input` string).
	MaxInputBytes = 8 << 20

	// MaxOutputBytes caps the emitted values in one result; a cut is never
	// silent, reported via Result.Truncated and Result.Note.
	MaxOutputBytes = 32 << 10
)

const (
	defaultMaxResults = 200

	maxResultsCeiling = 5000

	maxFilterBytes = 4 << 10

	maxInputDocs = 1000

	maxValueDepth = 200
)

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
	// `path` can be resolved (inline `input` still works).
	ErrNoWorkspaceRoot = errors.New("jq: no workspace root")
)

const maxEchoRunes = 120

func echoArg(s string) string {
	r := []rune(s)
	if len(r) > maxEchoRunes {
		return strconv.Quote(string(r[:maxEchoRunes])) + fmt.Sprintf("… (+%d more characters)", len(r)-maxEchoRunes)
	}
	return strconv.Quote(s)
}

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

func recoverablef(format string, a ...any) error {
	return errors.New(fmt.Sprintf(format, a...) + " " + severityRecoverable)
}

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
	// Source names what was queried: the workspace-relative path or the inline marker, never the host-absolute path.
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

func intFromFloat(f float64) (int, bool) {
	switch {
	case f != f:
		return 0, false
	case f >= float64(maxInt):
		return maxInt, true
	case f <= float64(minInt):
		return minInt, true
	}
	return int(f), true
}

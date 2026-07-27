// Package jqtool is the agent's jq engine: ONE tool, `jq_query`, that runs a
// real jq program over one JSON or YAML document and hands back the values it
// emits. It is built on github.com/itchyny/gojq (a pure-Go jq reimplementation,
// no CGO), so it ships wherever this binary ships.
//
// # The named problems it exists for
//
//   - CONTENOX'S OWN CONFIGURATION SURFACE IS JSON. Chain files, hitl-policy
//     files and agent definitions are all JSON documents this runtime reads,
//     and they are the documents an agent is most often asked to reason about.
//     An agent debugging a chain wants `.tasks[] | select(.handler=="tools")`.
//     Without this tool the only way to answer that is to read a 900-line file
//     into the context window and let the model do the filtering by eye — which
//     costs the whole file in tokens, every turn it stays in context, and gets
//     the answer wrong on the lines that scrolled past.
//
//   - THE SAME PROBLEM IN THE TS/JS DIRECTION. package.json, tsconfig.json,
//     package-lock.json, .eslintrc — the ecosystem TODO.md now weights toward
//     keeps its whole configuration surface in JSON, and a lockfile is
//     megabytes of exactly the shape jq was invented for.
//
//   - A JQ FILTER IS A FRACTION OF THE TOKENS OF THE EQUIVALENT PROGRAM.
//     `.tasks[]|select(.handler=="tools")|.id` is eleven tokens. The JavaScript
//     that does the same thing — read, parse, filter, project, stringify — is
//     an order of magnitude more, and every one of those tokens is a place to
//     get it wrong.
//
// # Why this is allow-tier BY CONSTRUCTION
//
// The envelope's rules are seeded `{tools: "jq", action: "allow"}`, and the
// justification is structural rather than a judgement call:
//
//   - IT READS A FILE THE AGENT MAY ALREADY READ. `path` goes through the same
//     vfs containment every other file-touching tool uses (see input.go), so
//     the set of bytes reachable here is a SUBSET of what local_fs.read_file —
//     itself allow-tier — already reaches.
//   - IT CANNOT WRITE. There is no write path in gojq and none in this package.
//   - IT CANNOT REACH THE NETWORK. jq has no I/O builtins at all; `input` and
//     `inputs` are refused at COMPILE time because no input iterator is
//     supplied, `import`/`include` are refused because no module loader is, and
//     `$ENV`/`env` compile to an empty object because the environ loader is
//     explicitly disabled (query.go). A jq program cannot observe this process.
//   - IT IS DETERMINISTIC AND FULLY DEADLINE-BOUNDED, INCLUDING RECURSION. This
//     is the property that makes the tier structural rather than optimistic: a
//     2026-07-27 spike confirmed that gojq's RunWithContext stops BOTH the
//     classic non-terminating program (`def f: f; f`) and a compute bomb
//     (`[range(1e8)]`) at the deadline. Regular expressions go through Go's
//     RE2 (linear time, no catastrophic backtracking).
//
// That last bullet is strictly stronger than the goja sandbox's guarantee. goja
// has no memory cap and its deadline bounds an allocation bomb by RATE rather
// than by ceiling, and its regexp fallback backtracks; here the deadline is a
// real bound on every execution path this package can reach.
//
// # What this is NOT
//
//   - NOT a data-mutation tool. jq programs that "modify" (`.a = 1`, `del(.b)`)
//     produce a NEW value in the result; nothing is ever written back to disk.
//   - NOT a replacement for goja_eval. See the overlap note on ToolQuery in
//     schema.go: jq is for declarative shape-work over ONE document; goja_eval
//     is for imperative compute and for anything that must call another tool.
//   - NOT a pipe. Its input is one workspace file or one inline string, both
//     size-capped. Pasting a megabyte of tool output into `input` spends the
//     tokens this tool exists to save.
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

// ToolsProviderName is the tools-provider key this package registers under (the
// `name` a chain's `tools` task, a policy rule, or a runtime allowlist refers
// to). Policy addressing is `tools: "jq"`.
//
// It is "jq" and not "json" or "query" on purpose: every model has seen jq, and
// the name alone tells it which language the `filter` argument is written in.
// A generic name would invite JSONPath, JMESPath and XPath in equal measure.
const ToolsProviderName = "jq"

// ToolQuery is the single tool this provider exposes. One tool, because jq
// itself is one verb — every operation (select, project, map, keys, length,
// group_by) is a program in the same language, not a separate entry point.
const ToolQuery = "jq_query"

// toolNames is the declaration order used by Supports and the tool list.
var toolNames = []string{ToolQuery}

// --- Limits -----------------------------------------------------------------
//
// Every limit is a documented constant and every one of them is asserted by a
// test. Four of them are exported because they are the numbers an operator or a
// caller has any reason to know; the rest are internal because nothing outside
// this package can act on them.
const (
	// DefaultDeadline bounds one jq_query execution. Same number, and the same
	// reason, as gojatool.DefaultDeadline: an interactive tool call that has not
	// answered in two seconds has stopped being interactive.
	//
	// This is the load-bearing bound of the whole package. It is what makes a
	// non-terminating filter (`def f: f; f`) and a compute bomb
	// (`[range(1e8)]`) into a two-second refusal instead of a hung session, and
	// therefore what makes allow tier defensible.
	DefaultDeadline = 2 * time.Second

	// MaxDeadline ceilings a per-call `deadline_ms` override. Same ceiling as
	// the goja sandbox, for the same reason: past half a minute a tool call is
	// no longer something a person is waiting on, and a filter that needs more
	// than that over a single capped document is the wrong filter.
	MaxDeadline = 30 * time.Second

	// MaxInputBytes caps the input document — the file behind `path` or the
	// inline `input` string.
	//
	// It is deliberately EIGHT TIMES local_fs's whole-file read cap
	// (defaultMaxReadBytes, 1 MiB). That is not an oversight, it is the point of
	// the tool: the bytes read here never enter the context window, only the
	// filter's OUTPUT does, so the honest limit is what this process can afford
	// to hold in memory for two seconds, not what a model can afford to read. A
	// 2 MB package-lock.json is exactly the document jq is for and exactly the
	// document a file read cannot help with.
	MaxInputBytes = 8 << 20

	// MaxOutputBytes caps the emitted values in one result. Same number as
	// gitMaxOutputBytes, and the same reason local_fs and git cap their own: one
	// tool result must not be able to dominate a small model's context window.
	// The cut is never silent — Result.Truncated and Result.Note say what was
	// withheld and what to narrow.
	MaxOutputBytes = 32 << 10
)

const (
	// defaultMaxResults is how many emitted values a call returns when `max` is
	// not given. A jq filter can emit unboundedly many values (`repeat(.)`,
	// `range(infinite)`), so a count cap is a separate necessity from the byte
	// cap: a stream of tiny values would otherwise spin against the byte cap for
	// the whole deadline.
	defaultMaxResults = 200

	// maxResultsCeiling ceilings a `max` override. A model asking for a million
	// values means "all of them"; it gets this many and is told so.
	maxResultsCeiling = 5000

	// maxFilterBytes caps the jq program itself. A legitimate ad-hoc filter is
	// tens of bytes; a 10 KB one is either a paste accident or an attempt to
	// spend the turn's budget in an argument. Refused before parsing, so the
	// echo in the error is clamped rather than the whole program.
	maxFilterBytes = 4 << 10

	// maxInputDocs caps how many DOCUMENTS one input may carry. A JSON input is
	// read as a stream (concatenated values / JSON Lines) and a YAML input as a
	// multi-document stream ("---"), because .jsonl logs and multi-doc k8s
	// manifests are both real and both exactly what jq is asked about. The
	// filter runs once per document, jq's own semantics.
	maxInputDocs = 1000

	// maxValueDepth bounds nesting when a decoded document is normalized into
	// the value types gojq accepts. Insurance against a hand-built deeply nested
	// document turning normalization into a stack overflow — a panic in a tool
	// call takes the process, a refusal takes the call.
	maxValueDepth = 200
)

// --- Errors -----------------------------------------------------------------
//
// Voice follows local_fs (internal/services/localtools/fs.go): a "jq: " prefix,
// the concrete value that failed, and the next call that would work. The
// severity marker is localtools' fatal-vs-recoverable convention
// (internal/services/localtools/hardening.go).
//
// Every failure this surface can produce is recoverable by a corrected call
// EXCEPT one: a session with no workspace root configured cannot read any
// `path`, and no argument fixes that (ErrNoWorkspaceRoot, input.go).
const (
	severityRecoverable = "(recoverable: adjust parameters and retry)"
	severityFatalToken  = "(fatal:"
)

// Sentinel errors, for errors.Is by callers that need to branch.
var (
	// ErrEscapesWorkspace means a `path` argument resolved outside the allowed
	// directory — traversal, an absolute path elsewhere, or a symlink inside the
	// workspace pointing out of it. ONE typed boundary for all three, so a
	// caller branching on it does not miss the two most obvious attempts.
	ErrEscapesWorkspace = errors.New("jq: path escapes allowed directory")

	// ErrNoWorkspaceRoot means no allowed directory was configured for this
	// session, so no `path` can be resolved at all. Inline `input` still works.
	ErrNoWorkspaceRoot = errors.New("jq: no workspace root")
)

// maxEchoRunes bounds how much of a MODEL-SUPPLIED string a teaching error
// quotes back. Same cap and same reason as gointel's: every argument on this
// surface is written by the model, so an error that echoes one verbatim is an
// output channel the model controls the length of.
const maxEchoRunes = 120

// echoArg renders a model-supplied argument for an error message: clamped, then
// Go-quoted, so control characters, NULs and bidi overrides are escaped rather
// than embedded in the result. Use it EVERYWHERE an argument is quoted back.
func echoArg(s string) string {
	r := []rune(s)
	if len(r) > maxEchoRunes {
		return strconv.Quote(string(r[:maxEchoRunes])) + fmt.Sprintf("… (+%d more characters)", len(r)-maxEchoRunes)
	}
	return strconv.Quote(s)
}

// echoErr renders a WRAPPED lower-level error inside a teaching message.
//
// Clamped for the reason gointel documents: the wrapped text routinely embeds
// the very argument that failed, so clamping the argument and then interpolating
// %v puts it straight back in through the side door. gojq's own parse and
// runtime messages are good enough to pass through — this only bounds them.
//
// It also SANITISES, which echoArg gets for free from %q and this does not. The
// same side door carries the same payload: `path: "a\x00b"` comes back from the
// filesystem as `lstat /root/a<NUL>b: invalid argument`, so a NUL or a bidi
// override the argument echo carefully escaped arrives raw inside the wrapped
// text. Non-printable runes become '?'; a newline is kept, because a multi-line
// parse error with its caret line is the most useful thing gojq produces.
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

// echoName renders a model-supplied IDENTIFIER — an argument NAME — for an
// error message. Same clamp as echoArg, non-printable runes replaced rather than
// embedded, but no quotes: the unknown-argument voice is local_fs's and reads as
// a bare comma-separated list.
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

// wrapRecoverable tags a sentinel-wrapping error recoverable unless it already
// carries a severity marker.
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

// --- Result -----------------------------------------------------------------

// Result is the tool's payload, returned as DataTypeJSON exactly the way
// gointel and searchtool return theirs.
//
// Values carries the emitted values as compact JSON — jq's own output, one
// entry per emitted value. They are json.RawMessage rather than pre-joined text
// on purpose: a string field would re-escape every quote in every value, which
// on this tool's typical answer (a list of small objects) roughly doubles the
// token cost of the thing the tool exists to make cheap.
//
// Count vs Truncated is the honesty pair, the same discipline searchtool's
// Found/Shown pair carries: Count is what came back, and Truncated plus Note say
// explicitly when a cap stopped the stream rather than the filter running out of
// values. A silent cut would let a model conclude "there are exactly 200
// matches" from a budget decision.
type Result struct {
	// Filter echoes the jq program, clamped. Cheap (filters are short) and it
	// makes a transcript readable without the tool call beside it.
	Filter string `json:"filter"`
	// Source names what was queried: the workspace-relative path, or the inline
	// marker. Never the host-absolute path.
	Source string `json:"source"`
	// Format is the parser that was actually used, "json" or "yaml" — stated
	// because it may have been SNIFFED rather than declared, and a filter that
	// answers oddly because the document parsed as the other format is a silent
	// wrong answer otherwise.
	Format string `json:"format"`
	// Documents is how many documents the input carried. 1 for an ordinary
	// file; more for JSON Lines or a multi-document YAML stream, where the
	// filter ran once per document — jq's own semantics, worth stating because
	// it explains a count nobody expected.
	Documents int `json:"documents"`
	// Values are the emitted values, compact JSON, in emission order.
	Values []json.RawMessage `json:"values"`
	// Count is len(Values).
	Count int `json:"count"`
	// Truncated marks a result a cap stopped. Never set without a Note saying
	// which cap and what to do about it.
	Truncated bool   `json:"truncated,omitempty"`
	Note      string `json:"note,omitempty"`
}

// appendNote joins result notes with a single space, so a result that hit two
// conditions says both things in one field.
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

// --- Numeric coercion --------------------------------------------------------

const (
	maxInt = int(^uint(0) >> 1)
	minInt = -maxInt - 1
)

// intFromFloat converts a JSON number to an int WITHOUT the undefined behaviour
// Go's float→int conversion has outside the integer range. A model that emits
// 1e30 or NaN for `max` is not a hypothetical — it is one bad completion — and
// int(1e30) is unspecified, so the clamp happens here rather than downstream.
// Out-of-range saturates; NaN reads as "no value", which takes the documented
// default. Every caller clamps again to its own ceiling.
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

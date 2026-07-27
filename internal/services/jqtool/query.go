package jqtool

// query.go is the bounded execution of one jq program.
//
// THE BOUNDS, AND WHY EACH ONE IS SEPARATE:
//
//   - THE DEADLINE bounds TIME, and it is the only bound that has to hold for
//     the allow tier to be honest. gojq checks the context between VM
//     instructions, so it stops a non-terminating program (`def f: f; f`) and a
//     compute bomb (`[range(1e8)]`) alike — the 2026-07-27 spike measured both
//     landing at the deadline rather than running away.
//   - THE RESULTS CAP bounds COUNT, because a filter can emit unboundedly many
//     values (`repeat(.)`) each of which is individually tiny; without it a
//     stream of `1`s would spin against the byte cap for the whole deadline.
//   - THE OUTPUT CAP bounds BYTES, because one enormous value is as bad for a
//     context window as a million small ones.
//
// THE ONE BOUND THAT IS SOFT, stated rather than hidden (gojatool documents the
// same class of gap for goja): the context is only observed BETWEEN VM
// instructions, so a single native builtin runs to completion. gojq's own
// ceiling on the worst of these — string repetition — is 2 GiB, above which it
// returns an error. Everything else this package can reach is bounded by the
// input cap and the deadline. If that ceiling ever matters in practice, the
// named escalation is a subprocess with an rlimit, not a knob here.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"

	"github.com/itchyny/gojq"
)

// request is one fully-resolved jq_query call.
type request struct {
	filter     string
	in         *loaded
	maxResults int
	deadline   time.Duration
}

// clampDeadline turns a caller-supplied millisecond budget into a duration,
// clamped rather than refused: a model asking for 10 minutes means "take your
// time", and it gets MaxDeadline and a result, not an argument error.
// The clamp happens on the MILLISECOND COUNT, before the multiplication, and
// that ordering is the whole point: time.Duration is an int64 of nanoseconds, so
// time.Duration(math.MaxInt) * time.Millisecond overflows to a NEGATIVE
// duration, and a negative timeout is an already-expired context. A model that
// emits `deadline_ms: 1e30` would otherwise have every query it made refused as
// "did not finish within its -1ms deadline" — a nonsense message about a bound
// nobody set. Found by the hostile-argument test, not by review.
func clampDeadline(ms int, ok bool) time.Duration {
	if !ok || ms <= 0 {
		return DefaultDeadline
	}
	if int64(ms) >= MaxDeadline.Milliseconds() {
		return MaxDeadline
	}
	return time.Duration(ms) * time.Millisecond
}

// clampResults bounds the requested value count the same way.
func clampResults(max int, ok bool) int {
	if !ok || max <= 0 {
		return defaultMaxResults
	}
	if max > maxResultsCeiling {
		return maxResultsCeiling
	}
	return max
}

// compile parses and compiles the filter.
//
// The two failures get DIFFERENT teaching shapes on purpose, because they have
// different fixes and a model that cannot tell them apart retries the wrong one:
// a parse error means the text is not jq at all (an unclosed bracket, a stray
// pipe), while a compile error means it IS jq but names something that does not
// exist (a misremembered builtin, an unbound $variable). gojq's own messages are
// precise — they carry the offending token and a column — so they are passed
// through rather than reworded, only clamped.
func compile(filter string) (*gojq.Code, error) {
	if strings.TrimSpace(filter) == "" {
		return nil, recoverablef(
			"jq: filter is required — pass a jq program, e.g. {\"filter\": \".tasks[] | select(.handler==\\\"tools\\\") | .id\"}. " +
				"Use \".\" to see the whole document")
	}
	if len(filter) > maxFilterBytes {
		return nil, recoverablef(
			"jq: the filter is %d bytes, over the %d-byte cap. A jq filter that long is almost always a paste accident; "+
				"if the transform really needs that much program, it is imperative work and belongs in goja_eval",
			len(filter), maxFilterBytes)
	}

	// A control character or a bidi override in the PROGRAM is refused before it
	// is parsed, and this is not tidiness — it is the silent-wrong-answer class.
	// gojq's lexer treats a NUL as end-of-input, so `.` + NUL + `[garbage` parses
	// CLEANLY as `.` and runs a different program than the one that was sent,
	// successfully, with no error anywhere to notice. A bidi override is the
	// display-side version of the same trick: a filter that reads one way in a
	// transcript and runs another. Newlines and tabs stay legal — a multi-line
	// jq program is ordinary.
	if r, bad := firstUnsafeRune(filter); bad {
		return nil, recoverablef(
			"jq: the filter contains the non-printing character U+%04X, which is refused: jq's lexer stops at a NUL, "+
				"so a filter carrying one silently runs only the part before it. Retype the filter as plain text "+
				"(newlines and tabs are fine)", r)
	}

	query, err := gojq.Parse(filter)
	if err != nil {
		return nil, recoverablef(
			"jq: the filter is not valid jq syntax: %s. Fix the PROGRAM, not the document — "+
				"check for an unclosed bracket, a trailing pipe, or shell quoting that ate a quote character. Filter was: %s",
			echoErr(err), echoArg(filter))
	}

	// COMPILE OPTIONS ARE THE CAPABILITY BOUNDARY, and the absences are as
	// load-bearing as the presence:
	//
	//   - WithEnvironLoader returning nil makes `env` and `$ENV` an EMPTY object.
	//     This is the one that matters. Without it a one-line filter — `$ENV` —
	//     reads every environment variable of this process, API keys included,
	//     straight into the model's context. gojq's default is already an empty
	//     map, but stating it here makes the guarantee load-bearing instead of
	//     incidental, and jqtool_test.go pins it.
	//   - NO WithInputIter, so `input`/`inputs` are refused at COMPILE time. A
	//     jq program cannot pull a second document from anywhere.
	//   - NO WithModuleLoader, so `import`/`include` cannot load a .jq file off
	//     disk. The filter is the whole program.
	//
	// What remains is pure computation over the value it was handed.
	code, err := gojq.Compile(query, gojq.WithEnvironLoader(func() []string { return nil }))
	if err != nil {
		return nil, recoverablef(
			"jq: the filter parses but does not compile: %s. The SYNTAX is fine — something it names does not exist "+
				"(a misremembered builtin, an unbound $variable, or `input`/`inputs`/`import`, none of which jq_query provides: "+
				"the document you pass is the only input). Filter was: %s",
			echoErr(err), echoArg(filter))
	}
	return code, nil
}

// execute runs the compiled filter over every input document and assembles the
// capped result.
func execute(ctx context.Context, req request) (*Result, error) {
	code, err := compile(req.filter)
	if err != nil {
		return nil, err
	}

	res := &Result{
		Filter:    clampEcho(req.filter),
		Source:    req.in.source,
		Format:    req.in.format,
		Documents: len(req.in.docs),
		Values:    []json.RawMessage{},
		Note:      req.in.note,
	}

	runCtx, cancel := context.WithTimeout(ctx, req.deadline)
	defer cancel()

	var (
		spent   int
		stopped bool
	)
	for _, doc := range req.in.docs {
		if stopped {
			break
		}
		iter := code.RunWithContext(runCtx, doc)
		for {
			v, ok := iter.Next()
			if !ok {
				break
			}
			if execErr, isErr := v.(error); isErr {
				return nil, runtimeError(execErr, req, res.Count)
			}
			raw, err := gojq.Marshal(v)
			if err != nil {
				// gojq.Marshal handles every value gojq can produce, so this is
				// a "cannot happen" that is reported rather than swallowed.
				return nil, recoverablef("jq: a value the filter emitted could not be rendered as JSON: %s", echoErr(err))
			}
			if spent+len(raw) > MaxOutputBytes {
				if res.Count == 0 {
					// One value, too big on its own. Returning nothing with a
					// concrete size beats returning a mangled prefix that is not
					// valid JSON and cannot be parsed by whoever reads it.
					return nil, recoverablef(
						"jq: the first value the filter emitted is %d bytes, over the %d-byte output cap. "+
							"Narrow the filter — `| keys` to see the shape, `| length` to count, `| .[0:5]` to sample, "+
							"or project the fields you need with `| {a, b}`",
						len(raw), MaxOutputBytes)
				}
				res.Truncated = true
				res.Note = appendNote(res.Note, fmt.Sprintf(
					"TRUNCATED at the %d-byte output cap after %d value(s); the filter had more to emit. "+
						"Narrow the projection (`| {a, b}`) or slice the stream (`| .[0:20]`).",
					MaxOutputBytes, res.Count))
				stopped = true
				break
			}
			res.Values = append(res.Values, json.RawMessage(raw))
			res.Count++
			spent += len(raw)
			if res.Count >= req.maxResults {
				res.Truncated = true
				res.Note = appendNote(res.Note, fmt.Sprintf(
					"TRUNCATED at the %d-value cap; the filter may have had more to emit. "+
						"Raise `max` (ceiling %d), or narrow the filter — `| length` counts without listing.",
					req.maxResults, maxResultsCeiling))
				stopped = true
				break
			}
		}
	}

	if res.Count == 0 && res.Note == "" {
		// An empty result is a real answer, and the commonest reason for one is
		// a filter that is correct jq about the wrong shape. Saying so costs one
		// line and saves the turn spent re-running the same filter.
		res.Note = "The filter matched nothing. This is an answer, not an error: no value satisfied it. " +
			"Check the document's actual shape with `keys` (objects), `length` (arrays), or `.` (everything)."
	}
	return res, nil
}

// runtimeError renders a failure that happened while the filter was RUNNING,
// which is the third and last of the three teaching shapes.
//
// The deadline is split out from every other runtime failure because it is the
// one whose fix is never "correct the filter's types": it means the program did
// not finish, and the two ways that happens — non-terminating recursion and
// materializing an enormous sequence — are both named, because a model that
// wrote `def f: f; f` will otherwise retry it verbatim.
func runtimeError(err error, req request, emitted int) error {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return recoverablef(
			"jq: the filter did not finish within its %s deadline (%d value(s) emitted before it was stopped). "+
				"Two filters never finish: one that recurses with no base case (`def f: f; f`), and one that materializes an "+
				"unbounded sequence (`[range(1e8)]`) — use `limit(n; …)` or `first(…)` instead. "+
				"Raise `deadline_ms` (ceiling %d) only if the work is genuinely large. Filter was: %s",
			req.deadline, emitted, MaxDeadline.Milliseconds(), echoArg(req.filter))

	case errors.Is(err, context.Canceled):
		return recoverablef("jq: the query was cancelled before it finished (%d value(s) emitted)", emitted)
	}

	var halt *gojq.HaltError
	if errors.As(err, &halt) {
		return recoverablef(
			"jq: the filter stopped itself with halt/halt_error: %s. That is the filter's own decision, not a fault of the document",
			echoErr(err))
	}

	return recoverablef(
		"jq: the filter is valid but failed on this document: %s. The PROGRAM and the DOCUMENT disagree about shape — "+
			"jq will not index an array with a string or iterate a scalar. Look at what is actually there first "+
			"(`.` for the whole value, `keys` for an object's fields, `type` for a value's kind), then correct the path. "+
			"Source was %s, filter was: %s",
		echoErr(err), echoArg(req.in.source), echoArg(req.filter))
}

// firstUnsafeRune returns the first rune that must not appear in a jq program,
// and whether there was one. Newline, tab and carriage return are allowed:
// multi-line filters are ordinary and none of them can truncate the lexer.
func firstUnsafeRune(s string) (rune, bool) {
	for _, r := range s {
		switch r {
		case '\n', '\t', '\r':
			continue
		}
		if !unicode.IsPrint(r) {
			return r, true
		}
	}
	return 0, false
}

// clampEcho renders the filter for the RESULT (not an error): clamped, but
// unquoted, because it is already a JSON string field.
func clampEcho(s string) string {
	r := []rune(s)
	if len(r) > maxEchoRunes {
		return string(r[:maxEchoRunes]) + "…"
	}
	return s
}

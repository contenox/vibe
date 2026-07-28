package jqtool

// query.go is the bounded execution of one jq program: the deadline bounds
// time, the results cap bounds count, and the output cap bounds bytes — three
// separate bounds because a filter can emit unboundedly many values, each
// individually tiny. The context is only checked between VM instructions, so
// a single native builtin (e.g. gojq's 2 GiB string-repetition ceiling) can
// still run to completion; everything else is bounded by the input cap and
// the deadline.

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
// clamped rather than refused. The clamp is applied to the millisecond count
// before multiplying: time.Duration is int64 nanoseconds, so multiplying an
// unclamped large ms value overflows to a negative duration (an
// already-expired context).
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

// compile parses and compiles the filter. Parse and compile errors get
// different messages on purpose: a parse error means the text isn't jq at
// all, a compile error means it's valid jq naming something that doesn't
// exist (an unbound $variable, a missing builtin).
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

	// Refused before parsing: gojq's lexer treats a NUL as end-of-input, so a
	// filter carrying one would parse cleanly as a truncated, different
	// program with no error to notice. A bidi override is the display-side
	// equivalent. Newlines and tabs remain legal.
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

	// The absent compile options are the capability boundary: WithEnvironLoader
	// returning nil makes `env`/`$ENV` an empty object (otherwise `$ENV` would
	// leak this process's environment, API keys included, into the model's
	// context); no WithInputIter means `input`/`inputs` are refused at compile
	// time; no WithModuleLoader means `import`/`include` cannot load a file.
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

// execute runs the compiled filter over every input document and assembles
// the capped result.
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
				// gojq.Marshal handles every value gojq can produce, so this
				// is a "cannot happen" reported rather than swallowed.
				return nil, recoverablef("jq: a value the filter emitted could not be rendered as JSON: %s", echoErr(err))
			}
			if spent+len(raw) > MaxOutputBytes {
				if res.Count == 0 {
					// One oversized value: return nothing with a concrete
					// size rather than an unparseable truncated prefix.
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
		// An empty result is a real answer, most often a correct filter over
		// the wrong shape — worth saying to save a repeat call.
		res.Note = "The filter matched nothing. This is an answer, not an error: no value satisfied it. " +
			"Check the document's actual shape with `keys` (objects), `length` (arrays), or `.` (everything)."
	}
	return res, nil
}

// runtimeError renders a failure that happened while the filter was running.
// The deadline case is split out because its fix is never "correct the
// filter's types": it names the two ways a filter fails to finish
// (non-terminating recursion, materializing an unbounded sequence).
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

// firstUnsafeRune returns the first rune that must not appear in a jq
// program, and whether there was one. Newline, tab and carriage return are
// allowed.
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

// clampEcho renders the filter for the result (not an error): clamped but
// unquoted, since it's already a JSON string field.
func clampEcho(s string) string {
	r := []rune(s)
	if len(r) > maxEchoRunes {
		return string(r[:maxEchoRunes]) + "…"
	}
	return s
}

package gojatool

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/dop251/goja"
)

// hostresult.go is THE PROGRAM-FACING RESULT CONTRACT: what a tool result turns
// into on the far side of host.tool.
//
// # The caveat this file exists to close
//
// A tool result is written for a READER. `git.git_status` answers with
// "branch main\nstaged for commit:\n  M x.go\n"; `local_fs.read_file` answers,
// when the same session already read the file, with "File unchanged since last
// read — …". Both are correct answers to a reader and neither is a contract.
// Live use (2026-07-27) found what happens when a PROGRAM consumes them: a
// script assumed git_status returned porcelain and reported "4 staged, 2 other,
// no untracked" for a tree with one modified and one untracked file, and another
// treated the unchanged-stub sentence as the file's content. Both were
// confidently wrong, both returned successfully, and nothing in the stack could
// have caught either — the mis-parse is invisible to everything except a human
// who already knows the answer.
//
// # The rule
//
// A value crossing into a script is exactly one of three things, and WHICH ONE
// is decided by the Go type the tool returned, not by a table this package keeps:
//
//	DATA  — any non-string Go value (a struct, a map, a slice, a number).
//	        Marshaled to JSON and handed over as a plain JS value: fields,
//	        indexes, iteration and JSON.stringify all work normally. The tool's
//	        Go type IS the shape; it is documented where the tool is. The one
//	        thing it refuses is being converted to a string, because JS answers
//	        that with "[object Object]" and no error.
//	TEXT  — a Go string. Handed over as ToolText: an OBJECT with `.text` on it,
//	        never a bare JS string.
//	NOTHING — nil, which is null.
//
// The whole point is the second one. `host.tool("git.git_status").split("\n")`
// used to be a silent mis-parse; it is now a TypeError, and every string
// operation a script is likely to reach for throws a teaching error naming the
// tool. An author who means to parse prose writes `.text` (or asks for
// `{raw: true}`) and owns that decision in a line of source anyone can read.
//
// # What this deliberately does NOT do
//
// There is no `host.toolShape(name)` pre-call table. This package cannot honestly
// answer "what shape does provider.tool return?" before the call: the engine's
// tool declarations describe ARGUMENTS, not results, and a table maintained here
// would be a second source of truth that drifts from the tools it describes — the
// exact class of error this file exists to remove. The VALUE declares its own
// shape instead (`v.shape === "text"`), which cannot drift because it is produced
// by the call it describes.

// programText is an OPTIONAL capability of a tool result, asserted structurally
// so no import is needed in either direction (the same trick taskengine's
// toolDiffProvider uses).
//
// It is implemented by a result whose model-facing rendering is a STAND-IN — a
// notice, a stub, a receipt — rather than the thing itself. local_fs.read_file's
// "File unchanged since last read" is the canonical case: for a model, whose
// earlier read is still in the conversation, the sentence is the whole answer;
// for a program, which has no conversation, it is a lie shaped like content.
// A result that implements this hands the program the real text.
type programText interface {
	// ProgramText returns what a PROGRAM caller should receive instead of the
	// rendered stand-in, and whether it is available.
	ProgramText() (string, bool)
}

// programUnusable is the other half of programText: a result whose stand-in has
// NO program-facing equivalent — a refusal, a denial, an "I did nothing and here
// is why". The bridge throws it rather than handing a script a sentence that
// looks like a successful answer. The returned string is the model-facing
// message, already written to teach.
type programUnusable interface {
	// ProgramUnusable returns the reason this result is not data, or "" when it
	// is usable after all.
	ProgramUnusable() string
}

// toolTextShape is the value of `.shape` on every ToolText. A script that wants
// to branch reads it; nothing else in this package does.
const toolTextShape = "text"

// hostResult converts one tool result into the value the script sees.
//
// raw is the author's explicit escape hatch (host.tool(name, args, {raw: true})):
// it returns the tool's value with no wrapping at all, so a string arrives as a
// bare JS string. It exists because parsing prose is sometimes exactly the right
// thing to do — reading a version out of a receipt, counting lines in a log — and
// a guard with no door is a guard people route around. What it buys is that the
// decision is IN THE SOURCE, at the call site, instead of being the default
// nobody chose.
func hostResult(codec jsonCodec, provider, tool string, raw bool, result any) (goja.Value, error) {
	if result == nil {
		return goja.Null(), nil
	}
	address := provider + "." + tool

	// A stand-in with no program-facing meaning is a refusal, not a result.
	// Checked before anything else, including raw: {raw: true} asks for the
	// exact value, and there is no exact value here — only a sentence about
	// why there isn't one.
	if u, ok := result.(programUnusable); ok {
		if reason := strings.TrimSpace(u.ProgramUnusable()); reason != "" {
			return nil, &unusableResultError{address: address, reason: reason}
		}
	}

	// A stand-in that CAN be redeemed hands over the real text.
	if p, ok := result.(programText); ok {
		if text, ok := p.ProgramText(); ok {
			result = text
		}
	}

	if s, ok := result.(string); ok {
		if raw {
			return codec.vm.ToValue(s), nil
		}
		return newToolText(codec.vm, address, s)
	}

	encoded, err := json.Marshal(result)
	if err != nil {
		return nil, err
	}
	value, err := codec.toJS(encoded)
	if err != nil || raw {
		return value, err
	}
	return guardDataPrimitive(codec.vm, address, value), nil
}

// guardDataPrimitive closes the other half of the mis-parse.
//
// A STRUCTURED result is a plain JS object, which is exactly what a script
// wants — until the script does `String(result).split("\n")` anyway, and JS
// hands it "[object Object]": one line, no error, a confident wrong answer. That
// is the same failure the ToolText wrapper exists to stop, arriving through the
// value that was supposed to be safe.
//
// So a data result refuses primitive conversion too. Only that: fields, indexes,
// iteration, spread, JSON.stringify and console.log all behave exactly as they
// would on any object, because those are how data is meant to be read.
func guardDataPrimitive(vm *goja.Runtime, address string, value goja.Value) goja.Value {
	obj, ok := value.(*goja.Object)
	if !ok {
		return value
	}
	throw := func(goja.FunctionCall) goja.Value {
		panic(jsError(vm, fmt.Sprintf(
			"goja: %s answered with structured DATA, and converting it to a string is not how to read it — JS would hand you \"[object Object]\" and a script that split it would answer confidently wrong. Read its fields (JSON.stringify(result) shows you which ones there are).",
			address)))
	}
	// Best effort: a value that refuses the definition is still valid data, and
	// failing the whole call over a missing guard would trade a wrong answer for
	// no answer.
	_ = obj.SetSymbol(goja.SymToPrimitive, vm.ToValue(throw))
	return obj
}

// unusableResultError carries a refusal out of hostResult so the bridge can
// throw it with the tool's own words. It is a distinct type rather than a
// formatted string so the bridge can tell it from a marshaling failure.
type unusableResultError struct {
	address string
	reason  string
}

func (e *unusableResultError) Error() string {
	return fmt.Sprintf("%s: %s produced no result a program can use: %s",
		ErrToolNotData, e.address, clampText(e.reason, maxErrorTextBytes))
}

func (e *unusableResultError) Unwrap() error { return ErrToolNotData }

// textTrapMethods are the string operations a script reaches for by reflex.
// Each is defined on ToolText as a function that throws, so the failure names
// the tool and the repair instead of being JS's "x.split is not a function" —
// which teaches nothing about WHY the value is not a string.
var textTrapMethods = []string{
	"split", "slice", "substring", "substr", "trim", "trimStart", "trimEnd",
	"indexOf", "lastIndexOf", "includes", "startsWith", "endsWith",
	"match", "matchAll", "replace", "replaceAll", "search",
	"toLowerCase", "toUpperCase", "concat", "padStart", "padEnd", "repeat",
	"charAt", "charCodeAt", "codePointAt", "normalize", "at",
}

// newToolText builds the guarded wrapper for a text result.
//
// The data properties are non-writable and non-configurable, so a script cannot
// paper over the guard by reassigning toString; they stay ENUMERABLE so
// JSON.stringify and console.log still show the value (returning the wrapper
// from run() must produce data, not a throw — the guard is about mistaking prose
// for a parseable string, not about touching it at all).
func newToolText(vm *goja.Runtime, address, text string) (goja.Value, error) {
	obj := vm.NewObject()

	data := []struct {
		name  string
		value any
	}{
		{"shape", toolTextShape},
		{"tool", address},
		{"text", text},
		{"bytes", len(text)},
	}
	for _, d := range data {
		if err := obj.DefineDataProperty(d.name, vm.ToValue(d.value), goja.FLAG_FALSE, goja.FLAG_FALSE, goja.FLAG_TRUE); err != nil {
			return nil, err
		}
	}

	throw := func(what string) func(goja.FunctionCall) goja.Value {
		return func(goja.FunctionCall) goja.Value {
			panic(jsError(vm, textTrapMessage(address, what)))
		}
	}
	for _, name := range textTrapMethods {
		if err := obj.DefineDataProperty(name, vm.ToValue(throw("."+name+"()")), goja.FLAG_FALSE, goja.FLAG_FALSE, goja.FLAG_FALSE); err != nil {
			return nil, err
		}
	}
	// The implicit conversions. With toString and valueOf both throwing,
	// String(v), `${v}`, v + "" and v.length all fail at the line that tried
	// rather than silently producing "[object Object]" — which is the same
	// silent-wrong-answer failure in a different costume.
	if err := obj.DefineDataProperty("toString", vm.ToValue(throw("converting it to a string")), goja.FLAG_FALSE, goja.FLAG_FALSE, goja.FLAG_FALSE); err != nil {
		return nil, err
	}
	if err := obj.DefineDataProperty("valueOf", vm.ToValue(throw("using it as a primitive")), goja.FLAG_FALSE, goja.FLAG_FALSE, goja.FLAG_FALSE); err != nil {
		return nil, err
	}
	if err := obj.SetSymbol(goja.SymToPrimitive, vm.ToValue(throw("using it as a primitive"))); err != nil {
		return nil, err
	}
	getter := vm.ToValue(throw(".length"))
	if err := obj.DefineAccessorProperty("length", getter, goja.Undefined(), goja.FLAG_FALSE, goja.FLAG_FALSE); err != nil {
		return nil, err
	}
	return obj, nil
}

// textTrapMessage is the one teaching error every trap raises. It names the
// tool, says what the value actually is, and gives both repairs — the deliberate
// one (.text) and the explicit one ({raw: true}) — because a script author who
// reaches this line is one edit away from a correct program either way.
func textTrapMessage(address, what string) string {
	return fmt.Sprintf(
		"goja: %s answered with TEXT written for a reader, not a data contract, so %s is refused — string surgery on prose is a guess that keeps working until the wording changes, and then answers confidently wrong. Read it deliberately with .text, or ask for the bare string with host.tool(%q, args, {raw: true}). Structured tools return objects instead, and those need no unwrapping.",
		address, what, address)
}

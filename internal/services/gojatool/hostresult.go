package gojatool

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/dop251/goja"
)

// hostResult (below) is the program-facing result contract: a value crossing
// into a script from host.tool is exactly one of DATA (any non-string Go
// value, marshaled to a plain JS value that refuses string conversion),
// TEXT (a Go string, wrapped as ToolText — an object whose string operations
// throw rather than let a script guess at prose that was never a contract),
// or NOTHING (nil, crossing as null). An author who means to parse prose asks
// for `.text` or `{raw: true}` explicitly.

// programText is an optional capability of a tool result, asserted
// structurally so no import is needed in either direction. It is implemented
// by a result whose model-facing rendering is a stand-in (a notice, a stub, a
// receipt) rather than the thing itself; a result that implements this hands
// the program the real text instead.
type programText interface {
	// ProgramText returns what a program caller should receive instead of
	// the rendered stand-in, and whether it is available.
	ProgramText() (string, bool)
}

// programUnusable is the other half of programText: a result whose stand-in
// has no program-facing equivalent (a refusal, a denial). The bridge throws
// it rather than handing a script a sentence shaped like a successful answer.
type programUnusable interface {
	// ProgramUnusable returns the reason this result is not data, or "" when
	// it is usable after all.
	ProgramUnusable() string
}

// toolTextShape is the value of `.shape` on every ToolText.
const toolTextShape = "text"

// hostResult converts one tool result into the value the script sees. raw is
// the author's explicit escape hatch (host.tool(name, args, {raw: true})): it
// returns the tool's value with no wrapping, so a string arrives as a bare JS
// string.
func hostResult(codec jsonCodec, provider, tool string, raw bool, result any) (goja.Value, error) {
	if result == nil {
		return goja.Null(), nil
	}
	address := provider + "." + tool

	// A stand-in with no program-facing meaning is a refusal, not a result.
	// Checked before raw, since there is no exact value here to hand over.
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

// guardDataPrimitive makes a data result refuse primitive conversion too, so
// `String(result)` cannot silently produce "[object Object]". Fields,
// indexes, iteration, spread, JSON.stringify and console.log are unaffected.
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
	// Best effort: failing the whole call over a missing guard would trade a
	// wrong answer for no answer.
	_ = obj.SetSymbol(goja.SymToPrimitive, vm.ToValue(throw))
	return obj
}

// unusableResultError carries a refusal out of hostResult so the bridge can
// distinguish it from a marshaling failure.
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
// Each is defined on ToolText as a function that throws, naming the tool and
// the repair instead of JS's uninformative "x.split is not a function".
var textTrapMethods = []string{
	"split", "slice", "substring", "substr", "trim", "trimStart", "trimEnd",
	"indexOf", "lastIndexOf", "includes", "startsWith", "endsWith",
	"match", "matchAll", "replace", "replaceAll", "search",
	"toLowerCase", "toUpperCase", "concat", "padStart", "padEnd", "repeat",
	"charAt", "charCodeAt", "codePointAt", "normalize", "at",
}

// newToolText builds the guarded wrapper for a text result. The data
// properties are non-writable and non-configurable, so a script cannot paper
// over the guard by reassigning toString, but stay enumerable so
// JSON.stringify and console.log still show the value.
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
	// With toString and valueOf both throwing, String(v), `${v}`, v + "" and
	// v.length all fail at the line that tried rather than silently
	// producing "[object Object]".
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

// textTrapMessage is the one teaching error every trap raises.
func textTrapMessage(address, what string) string {
	return fmt.Sprintf(
		"goja: %s answered with TEXT written for a reader, not a data contract, so %s is refused — string surgery on prose is a guess that keeps working until the wording changes, and then answers confidently wrong. Read it deliberately with .text, or ask for the bare string with host.tool(%q, args, {raw: true}). Structured tools return objects instead, and those need no unwrapping.",
		address, what, address)
}

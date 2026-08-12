package gojatool

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/dop251/goja"
)

type programText interface {
	ProgramText() (string, bool)
}

type programUnusable interface {
	ProgramUnusable() string
}

const toolTextShape = "text"

func hostResult(codec jsonCodec, provider, tool string, raw bool, result any) (goja.Value, error) {
	if result == nil {
		return goja.Null(), nil
	}
	address := provider + "." + tool

	// Checked before raw: an unusable stand-in has no exact value to hand over.
	if u, ok := result.(programUnusable); ok {
		if reason := strings.TrimSpace(u.ProgramUnusable()); reason != "" {
			return nil, &unusableResultError{address: address, reason: reason}
		}
	}

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
	_ = obj.SetSymbol(goja.SymToPrimitive, vm.ToValue(throw))
	return obj
}

type unusableResultError struct {
	address string
	reason  string
}

func (e *unusableResultError) Error() string {
	return fmt.Sprintf("%s: %s produced no result a program can use: %s",
		ErrToolNotData, e.address, clampText(e.reason, maxErrorTextBytes))
}

func (e *unusableResultError) Unwrap() error { return ErrToolNotData }

var textTrapMethods = []string{
	"split", "slice", "substring", "substr", "trim", "trimStart", "trimEnd",
	"indexOf", "lastIndexOf", "includes", "startsWith", "endsWith",
	"match", "matchAll", "replace", "replaceAll", "search",
	"toLowerCase", "toUpperCase", "concat", "padStart", "padEnd", "repeat",
	"charAt", "charCodeAt", "codePointAt", "normalize", "at",
}

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
	// toString/valueOf both throwing makes String(v), `${v}`, v+"" and v.length fail at the call site instead of yielding "[object Object]".
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

func textTrapMessage(address, what string) string {
	return fmt.Sprintf(
		"goja: %s answered with TEXT written for a reader, not a data contract, so %s is refused — string surgery on prose is a guess that keeps working until the wording changes, and then answers confidently wrong. Read it deliberately with .text, or ask for the bare string with host.tool(%q, args, {raw: true}). Structured tools return objects instead, and those need no unwrapping.",
		address, what, address)
}

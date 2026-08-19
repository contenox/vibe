// Package echotool is the wiring fixture: one tool, `echo`, that returns its
// input and nothing else. It reads no file, opens no socket and starts no
// process, so it is the cheapest proof that declaration gating admits a named
// native toolset, and the cheapest chain step that is not a model call.
package echotool

import (
	"errors"
	"fmt"
	"strings"
	"unicode"
)

// ToolsProviderName is the tools-provider key this package registers under; the
// native- prefix is a namespace, not a gate: it keeps a declared MCP source from
// minting the same key. An allowlist addresses this toolset by that exact name —
// "*" admits it like any other, "!native-echo" removes it. Policy addresses it
// as `tools_policies.native-echo`.
const ToolsProviderName = "native-echo"

// ToolEcho is the single tool this provider exposes. The name is the HITL
// policy key; renaming it is a policy change, not a refactor.
const ToolEcho = "echo"

var toolNames = []string{ToolEcho}

// nothingToEcho answers a call that carries no content, so the result is never
// empty: an empty tool result reads to a model as a failed call.
const nothingToEcho = "nothing to echo"

const severityRecoverable = "(recoverable: adjust parameters and retry)"

const maxEchoRunes = 120

// echoName clamps a model-supplied name before it is quoted back in an error.
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

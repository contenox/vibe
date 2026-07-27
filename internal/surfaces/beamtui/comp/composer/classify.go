package composer

import (
	"strings"
	"unicode"
)

// ShellPrefix is the character that routes a line to the session's shell
// instead of the model (D8: `!`, matching the implemented backend, its tests
// and its docs).
const ShellPrefix = '!'

// Kind is what a submitted buffer turns out to be. The composer is the only
// component that decides this; shell-pane and command-palette receive the
// already-classified line (blueprint 4.11 ownership ruling).
type Kind int

const (
	// KindChat is an ordinary prompt for the model.
	KindChat Kind = iota
	// KindCommand is a slash command. The composer only recognizes the
	// trigger; the server owns the command name space.
	KindCommand
	// KindShell is a line for the session's shell.
	KindShell
)

// String names the kind for logs, status text, and test output.
func (k Kind) String() string {
	switch k {
	case KindCommand:
		return "command"
	case KindShell:
		return "shell"
	default:
		return "chat"
	}
}

// Submission is one classified buffer, handed to the app-shell at submit.
//
// Text is always the full original buffer, verbatim — what gets persisted as
// the user's turn and what RestoreLast puts back. Payload is the part the
// owning consumer acts on: for chat and command that is the text unchanged
// (acpsvc's parseCommand does its own trimming and name matching), for shell
// it is everything after the `!`, left-trimmed.
type Submission struct {
	Kind    Kind
	Text    string
	Payload string
}

// Classify applies the blueprint's two triggers, shell before slash (MVP
// item 6), to arbitrary text. Submit uses it; the app-shell may also call it
// on the live draft to hint at what Enter will do.
//
// The rules, deliberately no stricter than the server's:
//
//   - Shell when the first non-whitespace rune is `!` AND some
//     non-whitespace follows it. A bare `!` is chat, so the prefix key alone
//     never swallows a turn (blueprint: "a bare prefix falls through as
//     chat").
//   - Command when the trimmed buffer starts with `/`. This mirrors the
//     trigger half of acpsvc's parseCommand and stops there: an unknown name
//     is still KindCommand and still goes down the normal prompt path, where
//     the server decides. Re-implementing the name list here would be a
//     second source of truth that goes stale the moment a command is added.
//   - Chat otherwise.
func Classify(text string) Submission {
	lead := strings.TrimLeftFunc(text, unicode.IsSpace)

	if strings.HasPrefix(lead, string(ShellPrefix)) {
		if payload := strings.TrimLeftFunc(lead[1:], unicode.IsSpace); payload != "" {
			return Submission{Kind: KindShell, Text: text, Payload: payload}
		}
		return Submission{Kind: KindChat, Text: text, Payload: text}
	}

	if strings.HasPrefix(lead, "/") {
		return Submission{Kind: KindCommand, Text: text, Payload: text}
	}

	return Submission{Kind: KindChat, Text: text, Payload: text}
}

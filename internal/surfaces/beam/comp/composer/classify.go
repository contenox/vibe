package composer

import (
	"strings"
	"unicode"
)

// ShellPrefix routes a line to the session's shell instead of the model.
const ShellPrefix = '!'

// Kind is what a submitted buffer turns out to be; only the composer decides
// it, and downstream components receive the already-classified line.
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
// Text is always the full buffer verbatim, persisted as the user's turn and
// what RestoreLast puts back. Payload is what the owning consumer acts on:
// unchanged for chat and command, or everything after `!`, left-trimmed,
// for shell.
type Submission struct {
	Kind    Kind
	Text    string
	Payload string
}

// Classify applies the shell-before-slash trigger rules to arbitrary text,
// no stricter than the server's. Submit uses it; the app-shell may also call
// it on the live draft to hint at what Enter will do.
//
//   - Shell: the first non-whitespace rune is `!` with non-whitespace
//     following it. A bare `!` is chat.
//   - Command: the trimmed buffer starts with `/`. An unknown name is still
//     KindCommand; the server decides what to do with it.
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

package term

import (
	"encoding/base64"
	"fmt"
	"io"
	"os"
	"strings"
	"unicode/utf8"
)

const (
	// clipboardPayloadCap is the largest base64 payload that survives the
	// common multiplexer limits (tmux's 74994-byte ceiling is the binding
	// one). Beyond it terminals silently drop the whole sequence, so beam
	// truncates and says so instead.
	clipboardPayloadCap = 74994

	// clipboardSourceCap is the matching source-byte budget: base64 expands
	// three bytes into four.
	clipboardSourceCap = clipboardPayloadCap / 4 * 3

	// screenChunkSize is GNU screen's per-DCS passthrough limit, measured
	// where screen measures it: on the wire, over the WHOLE sequence.
	screenChunkSize = 768

	// screenChunkBody is what is left for the payload once the DCS introducer
	// and string terminator are paid for. Budgeting the full 768 for the body
	// puts four bytes of every chunk past the limit, which is exactly the size
	// at which screen starts dropping sequences silently.
	screenChunkBody = screenChunkSize - len("\x1bP") - len("\x1b\\")
)

// CopyToClipboard sends text to the system clipboard with OSC 52,
// DCS-wrapped when a multiplexer is in the way. Fire-and-forget by nature: a
// nil error means the bytes were written, never that a clipboard accepted
// them. Environment is read per call, because a session can be re-attached
// under a different multiplexer than it started in.
func (e *ANSI) CopyToClipboard(text string) (truncated bool, err error) {
	seq, truncated := clipboardSequence(text, os.Getenv("TMUX") != "", os.Getenv("STY") != "")
	if _, werr := io.WriteString(e.out, seq); werr != nil {
		return truncated, fmt.Errorf("beam term: clipboard: %w", werr)
	}
	return truncated, nil
}

// clipboardSequence builds the escape sequence for one copy and reports
// whether the payload had to be cut. tmux wins over screen when both are
// signalled: screen inside tmux is the nesting that occurs in practice, and
// the outer multiplexer is the one that must pass the sequence on.
func clipboardSequence(text string, tmux, screen bool) (string, bool) {
	payload, truncated := clipboardPayload(text)
	osc := "\x1b]52;c;" + payload + "\a"
	switch {
	case tmux:
		// tmux passthrough forwards the body verbatim once every ESC in it
		// is doubled.
		return "\x1bPtmux;" + strings.ReplaceAll(osc, "\x1b", "\x1b\x1b") + "\x1b\\", truncated
	case screen:
		return screenPassthrough(osc), truncated
	}
	return osc, truncated
}

// clipboardPayload base64-encodes text, cutting it at a rune boundary when
// the encoding would exceed the cap.
func clipboardPayload(text string) (string, bool) {
	truncated := false
	if len(text) > clipboardSourceCap {
		cut := clipboardSourceCap
		for cut > 0 && !utf8.RuneStart(text[cut]) {
			cut--
		}
		text, truncated = text[:cut], true
	}
	return base64.StdEncoding.EncodeToString([]byte(text)), truncated
}

// screenPassthrough splits a sequence into DCS chunks screen will forward,
// each one whole — introducer, body and terminator — inside the limit. The
// body's only ESC is the OSC introducer at the very start, so no chunk
// boundary can land inside an escape sequence screen would misread.
func screenPassthrough(osc string) string {
	var b strings.Builder
	for len(osc) > 0 {
		n := min(screenChunkBody, len(osc))
		b.WriteString("\x1bP")
		b.WriteString(osc[:n])
		b.WriteString("\x1b\\")
		osc = osc[n:]
	}
	return b.String()
}

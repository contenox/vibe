package transcript

import "strings"

// sanitizer strips terminal control sequences (all SGR/CSI/OSC/DCS escapes
// and C0 controls except newline/tab) out of shell output so the transcript
// only ever holds literal cells: escape codes and cursor-motion sequences
// must never reach frame.Span text or scrollback. The parser is a state
// machine carried across chunk boundaries, so an escape split across two
// TerminalChunk events is still recognized as one sequence. Bytes >= 0x80
// always pass through as UTF-8 continuation bytes.
type sanitizer struct {
	state sanState
	// cr records a carriage return held back one byte, since "\r\n" is one
	// line ending but a lone "\r" is a cursor return, and the next byte that
	// disambiguates it may be in the next chunk.
	cr bool
}

type sanState uint8

const (
	sanText   sanState = iota // ordinary output
	sanEsc                    // ESC seen, dispatch byte pending
	sanCSI                    // CSI parameters, runs to a final byte 0x40..0x7E
	sanOSC                    // OSC string, runs to BEL or ST
	sanOSCEsc                 // ESC inside an OSC/DCS string, "\" would end it
	sanStr                    // DCS/SOS/PM/APC string, runs to ST
	sanStrEsc
	sanEscInt // ESC intermediate (charset select), one more byte to eat
)

// write returns the printable text of chunk. Newlines, tabs and carriage
// returns survive (the caller resolves CR against the line it lands on, see
// applyCR); every other C0 control and DEL is dropped.
func (s *sanitizer) write(chunk string) string {
	// The common case allocates nothing: nothing to strip, return as-is.
	if s.state == sanText && !s.cr && !needsStrip(chunk) {
		return chunk
	}
	var b strings.Builder
	b.Grow(len(chunk) + 1)
	for i := 0; i < len(chunk); i++ {
		c := chunk[i]
		if s.cr {
			// Resolve the held CR now that its successor is known; a held CR
			// can only come from sanText, so the state below is still sanText.
			s.cr = false
			if c == '\n' {
				b.WriteByte('\n')
				continue
			}
			b.WriteByte('\r')
		}
		switch s.state {
		case sanText:
			switch {
			case c == 0x1b:
				s.state = sanEsc
			case c == '\r':
				s.cr = true
			case c == '\n' || c == '\t':
				b.WriteByte(c)
			case c < 0x20 || c == 0x7f:
				// dropped: BEL, backspace, NUL and friends
			default:
				b.WriteByte(c)
			}
		case sanEsc:
			switch c {
			case '[':
				s.state = sanCSI
			case ']':
				s.state = sanOSC
			case 'P', 'X', '^', '_':
				s.state = sanStr
			case '(', ')', '*', '+', '#', '%':
				s.state = sanEscInt
			default:
				// Two-byte escape (RIS, IND, NEL, DECSC...): both bytes go.
				s.state = sanText
			}
		case sanCSI:
			if c >= 0x40 && c <= 0x7e {
				s.state = sanText
			}
		case sanOSC:
			switch c {
			case 0x07:
				s.state = sanText
			case 0x1b:
				s.state = sanOSCEsc
			}
		case sanOSCEsc:
			switch c {
			case '\\':
				s.state = sanText
			case 0x1b:
				// stay: a second ESC re-arms the terminator
			default:
				s.state = sanOSC
			}
		case sanStr:
			if c == 0x1b {
				s.state = sanStrEsc
			}
		case sanStrEsc:
			switch c {
			case '\\':
				s.state = sanText
			case 0x1b:
			default:
				s.state = sanStr
			}
		case sanEscInt:
			s.state = sanText
		}
	}
	return b.String()
}

// needsStrip reports whether chunk holds anything the sanitizer would remove
// or hold back; a CR counts, since it must pass through the state machine so
// a "\r\n" split across chunks is still one line ending.
func needsStrip(chunk string) bool {
	for i := 0; i < len(chunk); i++ {
		if c := chunk[i]; (c < 0x20 && c != '\n' && c != '\t') || c == 0x7f {
			return true
		}
	}
	return false
}

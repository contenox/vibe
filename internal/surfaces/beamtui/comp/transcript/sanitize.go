package transcript

import "strings"

// sanitizer strips terminal control sequences out of shell output so the
// transcript only ever holds literal cells — [frame.Span] text may not carry
// escape codes, and a cursor-motion sequence printed into scrollback would
// scribble over lines the terminal already owns.
//
// This is the MVP simplification of blueprint 4.17 requirement 7, which asks
// for SGR to be PARSED and re-rendered through the style table while
// everything else is stripped. Here SGR is stripped too: shell output renders
// in one StyleShell role, and colored program output loses its color rather
// than its content. What requirement 7 does get in full is the hard part —
// the parser is a state machine carried across chunk boundaries, so an escape
// split between two TerminalChunk events is still recognized as one sequence
// rather than leaking its tail into the transcript as text. That exact defect
// was found and fixed once already in the deleted web frontend's sanitizer.
//
// Bytes >= 0x80 always pass through: they are UTF-8 continuation bytes, never
// C1 controls, and treating them as controls is how a sanitizer eats non-ASCII
// text.
type sanitizer struct {
	state sanState
	// cr records a carriage return held back one byte. CR is not a control
	// to drop and not a line ending on its own: "\r\n" is ONE line ending
	// and a lone "\r" is a cursor return that overwrites its line. Which one
	// it is depends on the next byte, and that byte may be in the next chunk
	// — the same cross-chunk problem the escape parser above solves, for the
	// same reason.
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
// returns survive — newlines settle a line, a tab is column alignment the
// caller expands, and a CR is a cursor motion the caller resolves against the
// line it lands on (see applyCR) — every other C0 control and DEL is dropped.
func (s *sanitizer) write(chunk string) string {
	// The common case is a chunk with nothing to strip, in which case the
	// input is returned as-is and no buffer is allocated.
	if s.state == sanText && !s.cr && !needsStrip(chunk) {
		return chunk
	}
	var b strings.Builder
	b.Grow(len(chunk) + 1)
	for i := 0; i < len(chunk); i++ {
		c := chunk[i]
		if s.cr {
			// Resolve the held CR now that its successor is known. A CR can
			// only be held from sanText, and it is consumed on the very next
			// byte, so the state below is still sanText here.
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
// or hold back. A CR counts: it is not removed, but it must go through the
// state machine so a "\r\n" split across chunks is still one line ending.
func needsStrip(chunk string) bool {
	for i := 0; i < len(chunk); i++ {
		if c := chunk[i]; (c < 0x20 && c != '\n' && c != '\t') || c == 0x7f {
			return true
		}
	}
	return false
}

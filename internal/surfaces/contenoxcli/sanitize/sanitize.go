// Package sanitize is beam's gate between untrusted text and a rendered span. It
// removes ESC-initiated sequences whole, every remaining C0 control and DEL, and
// the Unicode bidi controls; bytes >= 0x80 always survive. The functions are
// stateless, so an escape split across streamed chunks loses only its ESC byte.
package sanitize

import (
	"strings"

	"github.com/contenox/contenox/internal/surfaces/contenoxcli/textwidth"
)

// DefaultTabStop is the tab width beam expands to.
const DefaultTabStop = 8

// Line returns s reduced to text safe for one span: tabs fold to a single space
// and no newline survives.
func Line(s string) string { return strip(s, false) }

// Lines returns s reduced to safe source text: the same removals as [Line],
// except "\n" and "\t" survive. Each resulting line must still go through
// [ExpandTabs] before becoming a span.
func Lines(s string) string { return strip(s, true) }

// ExpandTabs replaces every tab in s with spaces up to the next multiple of
// tabstop, measured in terminal cells. A non-positive tabstop falls back to
// [DefaultTabStop]. s must already be a single line.
func ExpandTabs(s string, tabstop int) string {
	if strings.IndexByte(s, '\t') < 0 {
		return s
	}
	if tabstop <= 0 {
		tabstop = DefaultTabStop
	}
	var b strings.Builder
	b.Grow(len(s) + tabstop)
	col := 0
	for _, r := range s {
		if r == '\t' {
			pad := tabstop - col%tabstop
			b.WriteString(strings.Repeat(" ", pad))
			col += pad
			continue
		}
		b.WriteRune(r)
		col += textwidth.Width(string(r))
	}
	return b.String()
}

// strip is the shared state machine; keepStructure is the Lines/Line difference.
func strip(s string, keepStructure bool) string {
	if !needsWork(s, keepStructure) {
		return s
	}

	var b strings.Builder
	b.Grow(len(s))

	// A sequence left open at the end of s eats the rest of s, the safe failure.
	const (
		text   = iota
		esc    // ESC seen, dispatch byte pending
		csi    // CSI parameters, runs to a final byte 0x40..0x7E
		osc    // OSC string, runs to BEL or ST
		oscEsc // ESC inside an OSC string; "\" would end it
		str    // DCS/SOS/PM/APC string, runs to ST
		strEsc
		escInt // ESC intermediate (charset select): one more byte to eat
	)
	state := text

	for _, r := range s {
		switch state {
		case text:
			switch {
			case r == 0x1b:
				state = esc
			case r == '\n':
				if keepStructure {
					b.WriteByte('\n')
				}
			case r == '\t':
				if keepStructure {
					b.WriteByte('\t')
				} else {
					b.WriteByte(' ')
				}
			case r < 0x20 || r == 0x7f:
			case isBidi(r):
			default:
				b.WriteRune(r)
			}
		case esc:
			switch r {
			case '[':
				state = csi
			case ']':
				state = osc
			case 'P', 'X', '^', '_':
				state = str
			case '(', ')', '*', '+', '#', '%':
				state = escInt
			default:
				// Two-byte escape (RIS, IND, NEL, DECSC...): both bytes go.
				state = text
			}
		case csi:
			if r >= 0x40 && r <= 0x7e {
				state = text
			}
		case osc:
			switch r {
			case 0x07:
				state = text
			case 0x1b:
				state = oscEsc
			}
		case oscEsc:
			switch r {
			case '\\':
				state = text
			case 0x1b:
				// A second ESC re-arms the terminator.
			default:
				state = osc
			}
		case str:
			if r == 0x1b {
				state = strEsc
			}
		case strEsc:
			switch r {
			case '\\':
				state = text
			case 0x1b:
			default:
				state = str
			}
		case escInt:
			state = text
		}
	}
	return b.String()
}

// needsWork reports whether s holds anything strip would change.
func needsWork(s string, keepStructure bool) bool {
	// The bidi controls are the only multibyte runes strip touches, and both
	// blocks share a lead byte, so the rune scan only runs when it might matter.
	maybeBidi := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 0x80 {
			if c == 0xe2 {
				maybeBidi = true
			}
			continue
		}
		if c == 0x7f || c < 0x20 {
			if keepStructure && (c == '\n' || c == '\t') {
				continue
			}
			return true
		}
	}
	return maybeBidi && strings.ContainsAny(s, bidiRunes)
}

// bidiRunes is the closed set strip removes.
const bidiRunes = "‪‫‬‭‮⁦⁧⁨⁩"

func isBidi(r rune) bool {
	return (r >= 0x202a && r <= 0x202e) || (r >= 0x2066 && r <= 0x2069)
}

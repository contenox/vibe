// Package sanitize is beam's one gate between untrusted text and a
// [frame.Span], which draws its text as literal cells with no interpretation
// beyond the style table. Left unsanitized, agent output, tool titles, file
// names or a diff could carry a cursor-motion sequence that scribbles over
// scrollback, an OSC that retitles the window or stuffs the clipboard, or a
// bidi override that makes a reviewed diff line read as the opposite of what
// it applies.
//
// Every ingest point runs text through here once, at the boundary where it
// arrives, so nothing downstream needs to re-check it. It removes:
//
//   - ESC-initiated sequences whole (CSI, OSC, DCS/SOS/PM/APC, charset
//     selects, two-byte escapes) — body and all, not just the ESC byte;
//   - every remaining C0 control and DEL;
//   - the Unicode bidi controls U+202A–U+202E and U+2066–U+2069.
//
// Bytes >= 0x80 always survive (UTF-8 continuation bytes, never C1 controls).
//
// [Line] and [Lines] differ only in what structure they keep: Line is for a
// value that becomes one span and strips newline/tab entirely (folding tabs
// to a space); Lines is for source text the caller will split itself, so it
// keeps "\n" and "\t" for [ExpandTabs] to handle once the caller knows which
// line a tab sits on.
//
// The functions are stateless: an escape split across streamed chunks loses
// only its ESC byte, so the tail arrives as literal text. The security
// property (no ESC ever reaches a span) still holds; the cosmetic one
// doesn't. Shell output goes through comp/transcript's stateful parser
// instead, for that reason.
package sanitize

import (
	"strings"

	"github.com/contenox/contenox/internal/surfaces/beam/textwidth"
)

// DefaultTabStop is the tab width beam expands to: 8, matching what every
// terminal, diff tool and pager already assumes.
const DefaultTabStop = 8

// Line returns s reduced to text safe for one [frame.Span]: escape
// sequences, C0 controls, DEL and bidi controls are removed, tabs fold to a
// single space, and no newline survives.
//
// Use it for values that render as a single line — names, titles,
// descriptions, error strings — where a newline breaks the line contract.
func Line(s string) string { return strip(s, false) }

// Lines returns s reduced to safe source text: the same removals as [Line],
// except "\n" and "\t" survive.
//
// Use it for text the caller splits itself (streamed prose, a user echo, a
// diff). Each resulting line must still go through [ExpandTabs] before
// becoming a span — a tab's width depends on the column it starts in.
func Lines(s string) string { return strip(s, true) }

// ExpandTabs replaces every tab in s with spaces up to the next multiple of
// tabstop, measured in terminal cells so wide runes land on the same stops a
// terminal would. A non-positive tabstop falls back to [DefaultTabStop].
//
// s is expected to be one line already: a newline does not reset the
// column, so a caller holding multi-line text must split it first.
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

// strip is the shared state machine. keepStructure decides whether "\n" and
// "\t" survive as themselves (Lines) or are dropped and folded to a space
// respectively (Line).
func strip(s string, keepStructure bool) string {
	if !needsWork(s, keepStructure) {
		return s
	}

	var b strings.Builder
	b.Grow(len(s))

	// state is the escape-sequence parser's position. It is local rather than
	// a field because these functions are stateless by contract; a sequence
	// left open at the end of s simply eats the rest of s, which is the safe
	// direction to fail.
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
				// dropped: CR, BEL, backspace, NUL and friends
			case isBidi(r):
				// dropped: invisible, and it reorders everything after it
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
				// stay: a second ESC re-arms the terminator
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

// needsWork reports whether s holds anything strip would change, so the
// overwhelmingly common clean string is returned without allocating.
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

// bidiRunes is the closed set strip removes: four legacy embedding/override
// controls, two isolate replacements, and the pop. All are zero-width and
// reorder everything after them — how a diff line can read as the reverse
// of what it applies.
const bidiRunes = "‪‫‬‭‮⁦⁧⁨⁩"

func isBidi(r rune) bool {
	return (r >= 0x202a && r <= 0x202e) || (r >= 0x2066 && r <= 0x2069)
}

// Package sanitize is beam's one gate between untrusted text and a
// [frame.Span].
//
// A span is drawn as LITERAL CELLS: the engine emits its text with no
// interpretation beyond the style table. That makes any escape sequence
// riding in on agent output, tool titles, file names, session names, command
// descriptions or a diff a direct write to the terminal — a cursor-motion
// sequence would scribble over scrollback the terminal already owns, an OSC
// could retitle the window or stuff the clipboard, and a bidi override could
// make a reviewed diff line read as the opposite of what it applies. None of
// that is hypothetical for a coding harness whose whole input surface is
// somebody else's repository.
//
// So every ingest point runs its text through here, once, at the boundary
// where the text ARRIVES rather than where it is drawn — a value that reached
// the state machine already clean cannot be forgotten by a later renderer.
//
// What is removed:
//
//   - ESC-initiated sequences, whole: CSI up to its final byte, OSC up to BEL
//     or ST, DCS/SOS/PM/APC up to ST, charset selects, and the two-byte
//     escapes. The BODY goes with the ESC — stripping only the ESC would
//     leave "[2J" as text, which is noise rather than a defect but is still
//     not what the agent said.
//   - every remaining C0 control and DEL.
//   - the Unicode bidi controls U+202A–U+202E and U+2066–U+2069, which are
//     invisible and reorder the text around them.
//
// Bytes >= 0x80 always survive: they are UTF-8 continuation bytes, never C1
// controls, and treating them as controls is how a sanitizer eats non-ASCII
// text.
//
// What is KEPT is the point of having two functions. [Line] is for a value
// that becomes one span — a session name, a tool title, an agent name — and
// leaves neither newline nor tab behind, because both break a line's cell
// arithmetic and a newline in a span violates [frame.Line]'s contract
// outright. [Lines] is for SOURCE text the caller will split itself: it keeps
// "\n" (structure, not a control) and keeps "\t" for the caller to expand
// with [ExpandTabs] once it knows the line the tab sits on. Collapsing a tab
// to one space is right for a name and wrong for a diff hunk or a column of
// shell output, which is exactly why the choice is the caller's.
//
// The functions are stateless, which is deliberate but has one consequence
// worth knowing: an escape SPLIT across two streamed chunks loses only its
// ESC byte, so the tail ("[2J") arrives as literal text. The security
// property — no ESC ever reaches a span — holds either way; the cosmetic one
// does not. Shell output, where split escapes are routine rather than
// pathological, goes through comp/transcript's stateful parser instead.
package sanitize

import (
	"strings"

	"github.com/contenox/beam/internal/surfaces/beamtui/textwidth"
)

// DefaultTabStop is the tab width beam expands to. Eight is what every
// terminal, diff tool and pager already assumes, so an expanded tab lines up
// with the same columns the operator would have seen running the command
// themselves.
const DefaultTabStop = 8

// Line returns s reduced to text safe for ONE [frame.Span]: escape sequences,
// C0 controls, DEL and bidi controls are removed, tabs fold to a single
// space, and no newline survives.
//
// Use it for every value that renders as a single line — names, titles,
// descriptions, hints, error strings — where a newline would break the line
// contract and a tab would break the width math.
func Line(s string) string { return strip(s, false) }

// Lines returns s reduced to safe SOURCE text: the same removals as [Line],
// except that "\n" and "\t" survive.
//
// Use it for text the caller splits into lines itself (streamed prose, a user
// echo, a mission report body, a diff). Every resulting line must still be
// run through [ExpandTabs] before it becomes a span — Lines deliberately does
// not decide what a tab is worth, because that depends on the column the line
// starts in.
func Lines(s string) string { return strip(s, true) }

// ExpandTabs replaces every tab in s with spaces up to the next multiple of
// tabstop, measuring in terminal CELLS so a line of wide runes lands on the
// stops a terminal would put it on. A non-positive tabstop falls back to
// [DefaultTabStop].
//
// s is expected to be one line already: a newline is treated as an ordinary
// rune and does NOT reset the column, because a caller holding multi-line
// text should be splitting it, not expanding it whole.
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
	// blocks encode with the same lead byte, so the expensive rune scan runs
	// only for a string that could plausibly hold one.
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

// bidiRunes is the closed set strip removes: the four legacy embedding and
// override controls plus their two isolate replacements and the pop. They are
// zero-width and change the reading order of everything after them, which is
// how a diff line can display as the reverse of what it applies.
const bidiRunes = "‪‫‬‭‮⁦⁧⁨⁩"

func isBidi(r rune) bool {
	return (r >= 0x202a && r <= 0x202e) || (r >= 0x2066 && r <= 0x2069)
}

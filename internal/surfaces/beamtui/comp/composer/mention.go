package composer

import (
	"unicode"

	"github.com/contenox/beam/internal/surfaces/beamtui/sanitize"
)

// The @-mention seam (blueprint MVP item 10). file-addressing owns finding,
// ranking, and resolving candidates; the composer stays the keystroke and
// textarea owner and exposes exactly two calls: read the token the caret is
// in, and splice a chosen replacement over it.
//
// Both use rune offsets into Draft() — the flat buffer text, newlines
// included — so a caller can check a span against the same string it read,
// and the pair stays meaningful across a multiline draft.

// MentionSpan reports the `@` token the caret is in.
//
// The token runs between whitespace boundaries, so the trigger rule falls
// out of the shape of the text rather than a second scan: a token is a
// mention only when its FIRST rune is `@`, which is true exactly when the
// `@` is at the buffer start or preceded by whitespace. "user@host" is one
// token beginning with 'u' and never triggers.
//
// start and length locate the token in Draft() (rune offsets, the `@`
// included); query is the token's text after the `@`, empty for a bare `@`
// the user has just typed. ok is false when the caret is not in a mention.
func (c *Composer) MentionSpan() (start, length int, query string, ok bool) {
	runes := c.draftRunes()
	pos := c.flatIndex()

	start = pos
	for start > 0 && !unicode.IsSpace(runes[start-1]) {
		start--
	}
	end := pos
	for end < len(runes) && !unicode.IsSpace(runes[end]) {
		end++
	}
	if start == end || runes[start] != '@' {
		return 0, 0, "", false
	}
	return start, end - start, string(runes[start+1 : end]), true
}

// SpliceMention replaces the span at start (rune offsets into Draft(), as
// returned by MentionSpan) with replacement plus one trailing space, and
// leaves the caret after that space — so selecting a completion lands the
// user mid-sentence, ready to keep typing.
//
// An out-of-range span is ignored rather than panicking: the completion that
// produced it is asynchronous and the buffer may have moved on. Like any
// edit it detaches the buffer from history recall.
func (c *Composer) SpliceMention(start, length int, replacement string) {
	runes := c.draftRunes()
	if start < 0 || length < 0 || start+length > len(runes) {
		return
	}
	// The replacement is a filename out of the workspace, which a filesystem
	// will happily let contain an escape sequence. Same gate as a paste.
	rep := []rune(sanitize.Line(replacement))
	out := concat(runes[:start], rep, []rune{' '}, runes[start+length:])
	c.setFlat(out, start+len(rep)+1)
	c.touch()
}

// draftRunes is the buffer flattened to runes, newlines included — the
// coordinate space MentionSpan and SpliceMention share with Draft().
func (c *Composer) draftRunes() []rune {
	return []rune(c.Draft())
}

// flatIndex is the caret's offset in draftRunes coordinates.
func (c *Composer) flatIndex() int {
	n := 0
	for i := 0; i < c.line; i++ {
		n += len(c.lines[i]) + 1 // + the newline
	}
	return n + c.off
}

// setFlat replaces the buffer from flat runes and places the caret at a flat
// offset. Newlines in runes become line breaks; sanitize already ran, so
// nothing else needs normalizing.
func (c *Composer) setFlat(runes []rune, caret int) {
	if caret < 0 {
		caret = 0
	}
	if caret > len(runes) {
		caret = len(runes)
	}

	lines := [][]rune{{}}
	line, off := 0, 0
	for i, r := range runes {
		if i == caret {
			line, off = len(lines)-1, len(lines[len(lines)-1])
		}
		if r == '\n' {
			lines = append(lines, []rune{})
			continue
		}
		lines[len(lines)-1] = append(lines[len(lines)-1], r)
	}
	if caret >= len(runes) {
		line, off = len(lines)-1, len(lines[len(lines)-1])
	}

	c.lines = lines
	c.line, c.off = line, off
}

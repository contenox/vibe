package composer

import (
	"unicode"

	"github.com/contenox/contenox/internal/surfaces/beam/sanitize"
)

// The @-mention seam: file-addressing finds, ranks, and resolves
// candidates; the composer only reads the token the caret is in and
// splices a replacement over it, both via rune offsets into Draft().

// MentionSpan reports the `@` token the caret is in: it is a mention only
// when its first rune is `@` (at the buffer start or preceded by
// whitespace), so "user@host" never triggers.
//
// start and length locate the token in Draft() (rune offsets, `@`
// included); query is its text after the `@`. ok is false outside a mention.
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

// SpliceMention replaces the span at start (as returned by MentionSpan)
// with replacement plus one trailing space, caret after it. An out-of-range
// span is ignored rather than panicking, since the async completion that
// produced it may be stale. Like any edit it detaches from history recall.
func (c *Composer) SpliceMention(start, length int, replacement string) {
	runes := c.draftRunes()
	if start < 0 || length < 0 || start+length > len(runes) {
		return
	}
	// A workspace filename may contain an escape sequence; sanitize it
	// like a paste.
	rep := []rune(sanitize.Line(replacement))
	out := concat(runes[:start], rep, []rune{' '}, runes[start+length:])
	c.setFlat(out, start+len(rep)+1)
	c.touch()
}

// draftRunes is the buffer flattened to runes, the coordinate space
// MentionSpan and SpliceMention share with Draft().
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

// setFlat replaces the buffer from flat runes and places the caret at a
// flat offset; newlines become line breaks.
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

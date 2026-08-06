package composer

import (
	"strconv"

	"github.com/contenox/contenox/internal/surfaces/beamtui/frame"
	"github.com/contenox/contenox/internal/surfaces/beamtui/textwidth"
)

// ASCIISigil is the composer's beam-bar in a Mono terminal, without its
// separating space — exported so testkit's glyph-parity test can check it
// against style's GlyphSet without this package importing style.
const ASCIISigil = "|"

const (
	// sigil is the mint beam-bar marking the composer, the same brand
	// device as the welcome header and status segment, plus its
	// separating space. Continuation rows keep the two cells so wrapped
	// text stays in one column.
	sigilUnicode = "▌ "
	sigilASCII   = ASCIISigil + " "
	continuation = "  "

	// upUnicode/upASCII head the scrolled-draft marker (see prefixSpan), the
	// same glyph comp/palette's footer uses for "more above".
	upUnicode = "↑"
	upASCII   = "^"

	// placeholder is the empty-buffer hint listing the three affordances a
	// user cannot discover by typing; ASCII swaps the middot for a hyphen.
	placeholderUnicode = "type / for commands · ! for shell · @ to attach"
	placeholderASCII   = "type / for commands - ! for shell - @ to attach"

	ellipsisUnicode = "…"
	ellipsisASCII   = "..."
)

// MinWidth is the narrowest terminal the composer's layout is guaranteed
// for: two cells of sigil plus two of content, the least that fits one wide
// rune beside the device. Below it a rune wider than the remaining content
// column still renders and may overflow by a cell (see fit) rather than
// deleting from the user's draft; at or above it the width invariant is
// exact.
const MinWidth = 4

// Render projects the buffer into terminal rows for width. Rows are the
// buffer soft-wrapped to width-2, hung off the two-cell sigil column: the
// first row carries the beam-bar (mint if focused, muted otherwise), other
// rows two muted spaces. Height is 1..MaxRows; a taller draft scrolls to
// keep the caret's row visible, and the first continuation row's gutter
// then counts what scrolled off the top (see prefixSpan). At or above
// MinWidth no returned line exceeds width.
//
// An empty buffer renders the placeholder hint, focused or not: focused IS
// the state that needs it, since an operator facing an empty prompt has no
// other way to learn that `/`, `!` and `@` mean anything. The caret rests on
// its first cell. Render also computes the caret CursorPos reports, so call
// it before reading the caret for a frame.
func (c *Composer) Render(width int, focused, ascii bool) []frame.Line {
	c.curRow, c.curCol = 0, prefixWidth

	if width <= 0 {
		c.curCol = 0
		return nil
	}

	contentW := width - prefixWidth
	if contentW < 1 {
		// Degenerate width: the sigil column is all there is; the caret has
		// nowhere of its own to go.
		c.curCol = width - 1
		return []frame.Line{{frame.S(sigilStyle(focused), fit(sigil(ascii), width))}}
	}

	if c.Empty() {
		return []frame.Line{{
			frame.S(sigilStyle(focused), sigil(ascii)),
			frame.S(frame.StyleMuted, clip(placeholder(ascii), contentW, ascii)),
		}}
	}

	rows, curRow, curCol := c.layout(contentW)

	// The caret can land one row past the text when the last row is exactly
	// full; only a focused composer has a caret to accommodate.
	if curRow >= len(rows) {
		if focused {
			rows = append(rows, "")
		} else {
			curRow = len(rows) - 1
		}
	}

	height := len(rows)
	if height > MaxRows {
		height = MaxRows
	}
	top := 0
	if focused && curRow >= height {
		top = curRow - height + 1
	}
	if limit := len(rows) - height; top > limit {
		top = limit
	}
	if top < 0 {
		top = 0
	}

	c.curRow, c.curCol = curRow-top, curCol
	if c.curRow < 0 {
		c.curRow = 0
	}
	if c.curRow > height-1 {
		c.curRow = height - 1
	}

	out := make([]frame.Line, 0, height)
	for i, row := range rows[top : top+height] {
		line := frame.Line{prefixSpan(i, top, focused, ascii)}
		if row != "" {
			line = append(line, frame.S(frame.StyleNone, fit(row, contentW)))
		}
		out = append(out, line)
	}
	return out
}

// CursorPos is the caret in the coordinates of the lines the most recent
// Render returned: a row index and a cell column that already includes the
// two-cell sigil prefix. Before the first Render it is an empty composer's
// caret. The app-shell offsets it by the region's origin and hides it when
// unfocused.
func (c *Composer) CursorPos() (row, col int) { return c.curRow, c.curCol }

// layout wraps every buffer line to contentW and returns the flat row list
// plus the caret's row and cell column (offset by the sigil prefix).
// Wrapping goes through textwidth.Wrap, which only inserts breaks, so the
// caret maps onto wrapped rows by counting runes without drift.
func (c *Composer) layout(contentW int) (rows []string, curRow, curCol int) {
	for i, l := range c.lines {
		wrapped := textwidth.Wrap(string(l), contentW)
		if i == c.line {
			r, col := locate(wrapped, c.off, contentW)
			curRow = len(rows) + r
			curCol = prefixWidth + col
		}
		rows = append(rows, wrapped...)
	}
	return rows, curRow, curCol
}

// locate maps a rune offset within one wrapped line onto a row index and
// cell column. An offset exactly on a wrap boundary belongs to the
// following row, since that is where the next typed rune appears; at the
// end of a full line it returns row len(wrapped), one past the line's own
// rows.
func locate(wrapped []string, off, contentW int) (row, col int) {
	cum := 0
	for i, r := range wrapped {
		rs := []rune(r)
		if off < cum+len(rs) {
			return i, textwidth.Width(string(rs[:off-cum]))
		}
		cum += len(rs)
	}
	last := len(wrapped) - 1
	w := textwidth.Width(wrapped[last])
	if w >= contentW {
		return len(wrapped), 0
	}
	return last, w
}

// prefixSpan is a row's sigil column: the beam-bar on the first row, two
// spaces on continuations; mint marks focus, muted the resting state.
//
// When the draft is taller than MaxRows, the first continuation row spends
// its two muted cells on a scroll marker ("↑3") instead, so a scrolled
// draft stays distinguishable from one that starts at the window; past nine
// hidden rows the count degrades to "↑+". Row 0 always keeps the beam-bar,
// since the device should keep identifying the region even once it scrolls.
func prefixSpan(row, top int, focused, ascii bool) frame.Span {
	switch {
	case row == 0:
		return frame.S(sigilStyle(focused), sigil(ascii))
	case row == 1 && top > 0:
		return frame.S(frame.StyleMuted, scrollMarker(top, ascii))
	}
	return frame.S(frame.StyleMuted, continuation)
}

// scrollMarker is the hidden-rows count in exactly prefixWidth cells.
func scrollMarker(hidden int, ascii bool) string {
	up := upUnicode
	if ascii {
		up = upASCII
	}
	if hidden > 9 {
		return up + "+"
	}
	return up + strconv.Itoa(hidden)
}

func sigilStyle(focused bool) frame.StyleID {
	if focused {
		return frame.StyleBrand
	}
	return frame.StyleMuted
}

func sigil(ascii bool) string {
	if ascii {
		return sigilASCII
	}
	return sigilUnicode
}

func placeholder(ascii bool) string {
	if ascii {
		return placeholderASCII
	}
	return placeholderUnicode
}

func ellipsis(ascii bool) string {
	if ascii {
		return ellipsisASCII
	}
	return ellipsisUnicode
}

// clip cuts s to w cells, marking the cut with an ellipsis when one fits.
func clip(s string, w int, ascii bool) string {
	if textwidth.Width(s) <= w {
		return s
	}
	tail := ellipsis(ascii)
	if textwidth.Width(tail) > w {
		tail = ""
	}
	return textwidth.Truncate(s, w, tail)
}

// fit is clip without the ellipsis, keeping the width invariant true; at
// every supported width (see MinWidth) the wrap already did the work and
// this returns s unchanged.
//
// Below MinWidth it declines to help: textwidth.Wrap cannot break a
// two-cell rune across two one-cell rows, so it overflows by one cell
// rather than cutting — truncating here would delete a character from the
// user's draft while CursorPos still counted it, desyncing the caret.
func fit(s string, w int) string {
	if textwidth.Width(s) <= w {
		return s
	}
	if cut := textwidth.Truncate(s, w, ""); cut != "" {
		return cut
	}
	return s
}

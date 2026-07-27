package composer

import (
	"strconv"

	"github.com/contenox/beam/internal/surfaces/beamtui/frame"
	"github.com/contenox/beam/internal/surfaces/beamtui/textwidth"
)

// ASCIISigil is the composer's beam-bar in a Mono terminal, without its
// separating space. It is exported so testkit's glyph-parity test can hold
// every surface's ASCII marker against the style package's GlyphSet in one
// place; components may not import style, so the agreement can only be
// checked from outside.
const ASCIISigil = "|"

const (
	// sigil is the gold beam-bar that marks the composer — the same brand
	// device as the welcome header and the status segment — plus its
	// separating space. Continuation rows keep the two cells so wrapped
	// text stays in one column.
	sigilUnicode = "▌ "
	sigilASCII   = ASCIISigil + " "
	continuation = "  "

	// upUnicode/upASCII head the scrolled-draft marker (see prefixSpan). The
	// same caret comp/palette's footer uses, so one glyph means "there is
	// more above" wherever the live region scrolls something.
	upUnicode = "↑"
	upASCII   = "^"

	// placeholder is the empty-and-unfocused hint: the three affordances a
	// user cannot discover by typing. The ASCII variant swaps the middot
	// for a hyphen — a non-ASCII rune must never reach a Mono terminal.
	placeholderUnicode = "type / for commands · ! for shell · @ to attach"
	placeholderASCII   = "type / for commands - ! for shell - @ to attach"

	ellipsisUnicode = "…"
	ellipsisASCII   = "..."
)

// MinWidth is the narrowest terminal the composer's layout guarantees are
// stated for: two cells of sigil column plus two of content, which is the
// least that can hold one wide rune beside the device.
//
// Below it the composer still renders and still never panics, but one
// guarantee is deliberately dropped: a rune wider than the remaining content
// column is emitted ANYWAY, so a line can overflow by a cell (see fit). The
// alternative is deleting characters out of the user's own draft, and a
// three-column terminal is not a reason to do that. Above MinWidth the width
// invariant is exact.
const MinWidth = 4

// Render projects the buffer into terminal rows for width.
//
// Rows are the buffer soft-wrapped to width-2, each hung off the two-cell
// sigil column: the first RENDERED row carries the beam-bar — gold when
// focused, muted when not — and every other row carries two muted spaces, so
// the device marks the region wherever a scrolled draft happens to start.
// The height is the rows needed, at least one and at most MaxRows; a taller
// draft scrolls so the caret's row stays visible, and the first continuation
// row's gutter then counts what scrolled off the top (see prefixSpan). At or
// above MinWidth no returned line is ever wider than width.
//
// Empty and unfocused renders the placeholder hint; empty and focused
// renders just the sigil, so a user who has come to type sees a clean caret
// instead of advice.
//
// Render also computes the caret CursorPos reports, so call it before
// reading the caret for a frame.
func (c *Composer) Render(width int, focused, ascii bool) []frame.Line {
	c.curRow, c.curCol = 0, prefixWidth

	if width <= 0 {
		c.curCol = 0
		return nil
	}

	contentW := width - prefixWidth
	if contentW < 1 {
		// Degenerate width: the sigil column is all there is, and the caret
		// has nowhere of its own to go.
		c.curCol = width - 1
		return []frame.Line{{frame.S(sigilStyle(focused), fit(sigil(ascii), width))}}
	}

	if c.Empty() {
		if focused {
			return []frame.Line{{frame.S(frame.StyleBrand, sigil(ascii))}}
		}
		return []frame.Line{{
			frame.S(frame.StyleMuted, sigil(ascii)),
			frame.S(frame.StyleMuted, clip(placeholder(ascii), contentW, ascii)),
		}}
	}

	rows, curRow, curCol := c.layout(contentW)

	// The caret can land one row past the text when the last row is exactly
	// full — the next rune typed starts a new row, so the caret needs one to
	// sit in. Only a focused composer has a caret to accommodate.
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
// Render returned: a row index into those lines and a cell column that
// already includes the two-cell sigil prefix. Before the first Render it is
// the caret of an empty composer.
//
// The app-shell offsets it by the composer region's origin and hides it when
// the composer is not focused.
func (c *Composer) CursorPos() (row, col int) { return c.curRow, c.curCol }

// layout wraps every buffer line to contentW and returns the flat row list
// plus the caret's row and cell column (the column already offset by the
// sigil prefix).
//
// Wrapping goes through textwidth.Wrap, which only ever INSERTS breaks — no
// rune is dropped or moved — so the caret maps onto the wrapped rows by
// counting runes, and the map cannot drift from what is drawn.
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

// locate maps a rune offset within one wrapped buffer line onto a row index
// and a cell column. An offset that sits exactly on a wrap boundary belongs
// to the following row — that is where the next typed rune will appear — so
// an offset at the end of a full line returns row len(wrapped), one past the
// line's own rows, and the caller decides what row that is.
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
// spaces on continuations. Gold marks focus; muted is the resting state.
//
// When the draft is taller than MaxRows the buffer scrolls, and the FIRST
// continuation row spends its two muted cells on a scroll marker instead —
// "↑3" for three rows hidden above the window. A draft that scrolled was
// otherwise indistinguishable from one that happened to start where the
// window does: the text above simply vanished with nothing on screen saying
// it existed. The marker rides inside the two cells the sigil column already
// costs, so nothing shifts and no content row is spent on chrome; past nine
// hidden rows the count degrades to "↑+", because two cells is two cells.
//
// Row 0 keeps the beam-bar unconditionally. It is the brand device marking
// the input region, and a region that stops identifying itself the moment it
// gets tall is worse than an unlabelled scroll count.
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

// fit is clip without the ellipsis: the guard that keeps the width invariant
// true. At every SUPPORTED width (see MinWidth) the wrap has already done the
// work and this returns s unchanged.
//
// Below the minimum it declines to help. contentW can fall to one cell there,
// and textwidth.Wrap cannot break a two-cell rune across two one-cell rows —
// it emits the rune and overflows, which is the only thing it can do. Cutting
// it here would turn that overflow into a DELETION: the composer would render
// a buffer with the user's character silently missing while CursorPos still
// counted it, so the caret would sit a cell off and backspace would appear to
// do nothing. Overflowing a 3-column terminal by one cell is a cosmetic
// problem. Eating what somebody typed is not.
func fit(s string, w int) string {
	if textwidth.Width(s) <= w {
		return s
	}
	if cut := textwidth.Truncate(s, w, ""); cut != "" {
		return cut
	}
	return s
}

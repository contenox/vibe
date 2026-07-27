package term

import (
	"bytes"
	"fmt"
	"io"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/contenox/beam/internal/surfaces/beamtui/frame"
	"github.com/contenox/beam/internal/surfaces/beamtui/textwidth"
)

// Escape sequences beam is allowed to emit. The list is deliberately short:
// no alternate screen, no mouse tracking, no full-screen erase (ED) and no
// absolute cursor addressing (CUP), because the engine paints inside a
// region it does not own the boundaries of. Blanking is therefore always
// EL2 plus relative cursor movement, one row at a time.
const (
	seqSyncBegin  = "\x1b[?2026h" // begin synchronized update
	seqSyncEnd    = "\x1b[?2026l" // end synchronized update
	seqCursorHide = "\x1b[?25l"
	seqCursorShow = "\x1b[?25h"
	seqClearLine  = "\x1b[2K" // EL 2: erase the whole current line
	seqPasteOn    = "\x1b[?2004h"
	seqPasteOff   = "\x1b[?2004l"
	seqFocusOn    = "\x1b[?1004h"
	seqFocusOff   = "\x1b[?1004l"
	seqBell       = "\a"
)

func cursorUp(n int) string {
	if n <= 0 {
		return ""
	}
	return "\x1b[" + strconv.Itoa(n) + "A"
}

func cursorDown(n int) string {
	if n <= 0 {
		return ""
	}
	return "\x1b[" + strconv.Itoa(n) + "B"
}

func cursorRight(n int) string {
	if n <= 0 {
		return ""
	}
	return "\x1b[" + strconv.Itoa(n) + "C"
}

// painter turns Frames into terminal bytes. It is the whole commit
// algorithm with none of the terminal syscalls: size is injected and output
// goes to any io.Writer, so every rendering invariant is testable headless.
//
// Coordinates are relative throughout. The painter knows the live region's
// origin only as "the row the cursor was on when this region started", and
// tracks its own physical row within the region; it never assumes a screen
// row, which is what lets scrollback scroll the terminal naturally.
//
// Auto-margin assumption (the one thing the painter believes about the
// terminal it cannot see): DECAWM is on and the wrap is DEFERRED — writing
// the last cell of a row leaves the cursor on that row with a pending-wrap
// flag, and the next printable rune, not the last one, is what moves to the
// following row. CR (and therefore every "\r\n" and every "\r" + EL2 the
// painter emits) cancels a pending wrap without consuming a row. Every
// modern terminal in beam's target set behaves this way (xterm's
// last-column flag and its descendants: VTE, kitty, alacritty, WezTerm,
// iTerm2, Windows Terminal/ConPTY, tmux). The consequences the painter
// depends on: a live row rendered to exactly `width` cells still occupies
// exactly one physical row, and a scrollback line of exactly `width` cells
// followed by "\r\n" produces one row of history and no blank row after it.
//
// The invariant that follows from it: the painter never emits an ERASE while
// a wrap is pending — EL2 would erase the row the cursor is still parked on
// rather than the row the text belongs to — and it does not have to, because
// every erase it emits is immediately preceded by CR, which resolves the flag
// first. Cursor MOVES need no such care: a move resolves the pending wrap as
// a side effect and lands on the row the painter's own model says it is on,
// which is why moveToRow is safe to emit at any point in a frame.
type painter struct {
	out    io.Writer
	styles StyleResolver

	width  int
	height int

	prev     []string     // rendered rows of the last committed live region
	prevRows int          // physical rows that region occupies on screen
	row      int          // physical cursor row, 0-based within the live region
	cursor   frame.Cursor // cursor state placed by the last commit
	painted  bool         // a live region exists on screen
	invalid  bool         // the next commit must fully repaint
}

// resize records a new terminal size and DISOWNS the live region, exactly as
// reset() does.
//
// Invalidating the diff cache is not enough. Every terminal in beam's target
// set reflows on resize: the rows the painter measured at the old width are
// rewrapped, so both the row count it remembers and the anchor it would move
// up to are wrong. Moving over them and repainting in place is how a resize
// produces interleaved fragments of two frames. So the painter forgets the
// region instead: the next commit paints fresh wherever the cursor now is and
// reclaims nothing.
//
// The accepted artifact is that one stale copy of the region may be left
// above the new one — a snapshot of the composer scrolled into history. That
// is honest (it looks like output, because it is) and strictly better than
// erasing rows the painter can no longer locate.
func (p *painter) resize(width, height int) {
	if width == p.width && height == p.height {
		return
	}
	p.width, p.height = width, height
	p.reset()
}

// reset forgets the live region entirely: the next commit paints fresh at
// the cursor's current position without reclaiming rows above it. Used after
// Suspend, where another program owned the screen and nothing about the old
// region is still true, and after a resize, where the terminal reflowed the
// region out from under the painter (see resize).
func (p *painter) reset() {
	p.prev = nil
	p.prevRows = 0
	p.row = 0
	p.cursor = frame.Cursor{}
	p.painted = false
	p.invalid = true
}

// commit renders one frame: Scrollback lines are printed once into terminal
// history above the live region, then the live region is repainted —
// minimally when only its rows changed — inside one synchronized-output
// bracket with the cursor hidden. A frame that changes nothing writes no
// bytes at all.
//
// The scrollback path is deliberately not row-arithmetic. Printing history
// RAW is constitutional (see renderRaw), so how many physical rows a commit
// prints is unknowable to the painter — it depends on the terminal's
// wrapping. The path is therefore built to never need the answer:
//
//  1. move to live row 0 and blank the ENTIRE previous live region (EL2 per
//     row, moving down, then back up). Nothing stale can survive below,
//     however far the printing that follows scrolls the screen.
//  2. print each scrollback line as CR + EL2 + raw text + "\r\n". The
//     terminal soft-wraps whatever is too wide; a soft-wrapped line stays
//     ONE logical line for native selection. The EL2 is redundant for the
//     rows step 1 blanked and is kept for the one path where step 1 blanks
//     nothing: after reset() (Suspend) the painter deliberately owns no
//     rows, so the row a history line starts on may still hold another
//     program's output. It is always emitted at column 0 of a row this
//     commit is about to fill, so it can neither clobber a pending wrap nor
//     erase anything beam wants to keep. Continuation rows of a
//     soft-wrapped line are NOT erased — that would mean knowing the wrap
//     the whole design refuses to predict; the terminal supplies blank rows
//     when it scrolls, and beam never prints above its own last output.
//  3. after the last "\r\n" the cursor sits at column 0 of the row below all
//     printed content, whatever wrapping occurred. That row is the live
//     region's new origin: re-anchor there and paint every live row fresh.
//     The diff cache is invalid by definition on this path, and no vacated
//     rows remain to clear — step 1 already blanked them.
func (p *painter) commit(f frame.Frame) error {
	live, cursor := f.Live, f.Cursor
	if p.height > 0 && len(live) > p.height {
		dropped := len(live) - p.height
		live = live[dropped:] // invariant: a too-tall region shows its tail
		// The frame's cursor is addressed in Live's coordinates; the rows the
		// clamp dropped are not on screen, so the caret has to move up with
		// them or it lands on the wrong line. placeCursor clamps what is left
		// into the region, which is what puts a caret in a scrolled-off row at
		// the top of the visible tail rather than at its bottom.
		cursor.Row -= dropped
	}
	rows := make([]string, len(live))
	for i, line := range live {
		rows[i] = p.renderLine(line, p.width)
	}

	history := make([]string, 0, len(f.Scrollback))
	for _, line := range f.Scrollback {
		history = append(history, p.renderRaw(line))
	}

	full := p.invalid || !p.painted || len(p.prev) == 0 || len(rows) == 0
	if len(history) == 0 && !full && sameRows(p.prev, rows) && p.cursor == cursor {
		return nil
	}

	anchor := p.row // where the last frame that reached the screen left us
	var b bytes.Buffer
	b.WriteString(seqSyncBegin)
	b.WriteString(seqCursorHide)
	occupied := max(len(rows), 1) // an empty region still owns one blanked row
	switch {
	case len(history) > 0:
		p.moveToRow(&b, 0)
		p.blankRegion(&b)
		for _, line := range history {
			b.WriteString("\r")
			b.WriteString(seqClearLine)
			b.WriteString(line)
			b.WriteString("\r\n")
		}
		p.row = 0 // re-anchor: the origin is wherever printing left the cursor
		occupied = p.paintAll(&b, rows)
	case full:
		p.moveToRow(&b, 0)
		occupied = p.paintAll(&b, rows)
		p.clearVacated(&b, len(p.prev)-occupied)
	default:
		p.paintDiff(&b, rows)
	}
	p.placeCursor(&b, cursor, len(rows))
	b.WriteString(seqSyncEnd)

	// One Write per commit, and the bookkeeping is published only once that
	// Write has fully succeeded. Publishing first would describe a frame the
	// screen may not be showing: a short or failed write can leave any prefix
	// of the frame painted, and a cache claiming otherwise makes the identical
	// retry a no-op — the diff finds nothing to do and the terminal keeps a
	// half-drawn frame forever.
	if _, err := p.out.Write(b.Bytes()); err != nil {
		// What is on screen is now unknown, so the next commit must repaint
		// everything; prev/prevRows/painted keep describing the last frame
		// that did land, and the anchor is rewound to match them. Neither is
		// certainly true of the screen — nothing can be after a partial write
		// — but they are the only self-consistent state left, and the full
		// repaint is what recovers from it.
		p.invalid = true
		p.row = anchor
		// The frame opened a synchronized-output bracket and hid the cursor.
		// If the write died between those and the end of the frame, the
		// terminal is frozen and blind until something closes them; this is
		// the only chance to try.
		_, _ = io.WriteString(p.out, seqSyncEnd+seqCursorShow)
		return fmt.Errorf("beam term: commit: %w", err)
	}

	p.prev = rows
	p.prevRows = occupied
	p.cursor = cursor
	p.painted = true
	p.invalid = false
	return nil
}

// blankRegion erases every physical row the live region currently occupies
// and returns to the first of them, leaving the cursor at column 0. It is
// the scrollback path's substitute for row arithmetic: once the region is
// blank, printing history over it cannot expose a fragment of the old
// composer no matter how the terminal wraps or scrolls, and no row the
// region vacates has to be tracked down afterwards.
//
// Only EL2 and relative cursor movement are used — ED and CUP stay
// forbidden — and every erase is preceded by CR, so a pending wrap is
// always resolved by that CR rather than clobbered by the erase.
func (p *painter) blankRegion(b *bytes.Buffer) {
	for i := range p.prevRows {
		if i > 0 {
			b.WriteString(cursorDown(1))
		}
		b.WriteString("\r")
		b.WriteString(seqClearLine)
	}
	b.WriteString(cursorUp(p.prevRows - 1))
}

// clear erases every physical row the region owns and forgets the region,
// leaving the cursor at column 0 of the row where the region began — the
// clean line the shell's next prompt lands on. This is Close's structural
// guarantee: no beam chrome (gutter, status bar) can survive an orderly
// exit, no matter what the final frame held or whether the app remembered
// to commit an empty one.
func (p *painter) clear() error {
	if p.prevRows == 0 {
		return nil
	}
	var b bytes.Buffer
	p.moveToRow(&b, 0)
	p.blankRegion(&b)
	b.WriteString("\r")
	p.reset()
	_, err := p.out.Write(b.Bytes())
	return err
}

// paintAll rewrites every live row from the current row downward and reports
// how many physical rows the region now occupies. Rows below the last one
// that exists are created with newlines, which is what scrolls the terminal.
func (p *painter) paintAll(b *bytes.Buffer, rows []string) int {
	if len(rows) == 0 {
		b.WriteString("\r")
		b.WriteString(seqClearLine)
		p.row = 0
		return 1
	}
	for i, row := range rows {
		if i > 0 {
			b.WriteString("\r\n")
		}
		b.WriteString("\r")
		b.WriteString(seqClearLine)
		b.WriteString(row)
		p.row = i
	}
	return len(rows)
}

// paintDiff rewrites only the rows whose rendering changed, grows the region
// with newlines when it got taller, and clears the rows it vacated when it
// got shorter.
func (p *painter) paintDiff(b *bytes.Buffer, rows []string) {
	old := len(p.prev)
	for i := 0; i < len(rows) && i < old; i++ {
		if rows[i] == p.prev[i] {
			continue
		}
		p.moveToRow(b, i)
		b.WriteString("\r")
		b.WriteString(seqClearLine)
		b.WriteString(rows[i])
	}
	switch {
	case len(rows) > old:
		p.moveToRow(b, old-1)
		for i := old; i < len(rows); i++ {
			b.WriteString("\r\n\r")
			b.WriteString(seqClearLine)
			b.WriteString(rows[i])
			p.row = i
		}
	case len(rows) < old:
		p.moveToRow(b, len(rows)-1)
		p.clearVacated(b, old-len(rows))
	}
}

// clearVacated blanks n rows below the cursor and returns to it. Cursor-down
// is used rather than newlines: these rows already exist, and scrolling here
// would drag the live region off the bottom of the screen.
func (p *painter) clearVacated(b *bytes.Buffer, n int) {
	if n <= 0 {
		return
	}
	for range n {
		b.WriteString(cursorDown(1))
		b.WriteString("\r")
		b.WriteString(seqClearLine)
	}
	b.WriteString(cursorUp(n))
}

// placeCursor parks the caret at the frame's cursor and shows it. A hidden
// cursor is simply left hidden wherever painting ended.
func (p *painter) placeCursor(b *bytes.Buffer, c frame.Cursor, height int) {
	if c.Hidden {
		return
	}
	if height == 0 {
		// An empty live region has no row to address, but every frame hides
		// the cursor on the way in. Leaving it hidden here would hand the user
		// an invisible caret for as long as the region stays empty.
		b.WriteString(seqCursorShow)
		return
	}
	row := c.Row
	if row < 0 {
		row = 0
	}
	if row > height-1 {
		row = height - 1
	}
	p.moveToRow(b, row)
	col := c.Col
	if col < 0 {
		col = 0
	}
	if p.width > 0 && col > p.width-1 {
		col = p.width - 1
	}
	b.WriteString("\r")
	b.WriteString(cursorRight(col))
	b.WriteString(seqCursorShow)
}

// moveToRow moves vertically to a row of the live region; every write starts
// with a carriage return, so the column never has to be tracked.
func (p *painter) moveToRow(b *bytes.Buffer, target int) {
	if target < 0 {
		target = 0
	}
	switch {
	case target < p.row:
		b.WriteString(cursorUp(p.row - target))
	case target > p.row:
		b.WriteString(cursorDown(target - p.row))
	}
	p.row = target
}

// renderLine resolves every span through the StyleResolver and truncates the
// result to the terminal width. Live rows are truncated rather than wrapped
// on purpose: the region's height is len(Live) by contract, and silently
// growing it would corrupt every offset the app computed. That geometry
// contract is why the live region keeps its width clamp while scrollback
// (renderRaw) deliberately loses it: live rows are repainted, never copied
// out of history, so truncation costs nothing a redraw does not fix.
func (p *painter) renderLine(line frame.Line, width int) string {
	if width <= 0 {
		return ""
	}
	var b strings.Builder
	remaining := width
	for _, s := range line {
		if remaining <= 0 {
			break
		}
		text := spanText(s.Text)
		if textwidth.Width(text) > remaining {
			text = textwidth.Truncate(text, remaining, "")
		}
		if text == "" {
			continue
		}
		prefix, suffix := p.sgr(s.Style)
		b.WriteString(prefix)
		b.WriteString(text)
		b.WriteString(suffix)
		remaining -= textwidth.Width(text)
	}
	return b.String()
}

func (p *painter) sgr(id frame.StyleID) (string, string) {
	if p.styles == nil {
		return "", ""
	}
	return p.styles.SGR(id)
}

// renderRaw resolves a scrollback line's spans and hands the result to the
// terminal WHOLE: no truncation, no wrapping, not even at the terminal's
// width. This is the copy/paste ruling made mechanical (blueprint section 1,
// acceptance test 1). A line wider than the terminal is soft-wrapped by the
// terminal itself, which keeps it ONE logical line in the selection model,
// so dragging over a long code line and pasting it yields exactly that line.
// The painter used to hard-wrap here instead, emitting "\r\n" at each wrap
// point; those are real line breaks in terminal history, and they came back
// out of every paste as phantom newlines in the middle of the user's code.
//
// The price is that the painter cannot know how many physical rows a
// scrollback line consumed — which is why commit's scrollback path is built
// to never ask.
func (p *painter) renderRaw(line frame.Line) string {
	var b strings.Builder
	for _, s := range line {
		text := spanText(s.Text)
		if text == "" {
			continue
		}
		prefix, suffix := p.sgr(s.Style)
		b.WriteString(prefix)
		b.WriteString(text)
		b.WriteString(suffix)
	}
	return b.String()
}

// spanText enforces frame.Span's "printable cells only" contract instead of
// trusting it. The engine is the last place a byte can turn into terminal
// behaviour, and the whole rendering model rests on beam emitting a closed
// list of escape sequences: one ESC smuggled through a span could clear the
// screen, enable the alternate screen or the mouse, or move the cursor out of
// the region the painter believes it owns — and a lone \n or \t would silently
// break the one-span-row-is-one-terminal-row geometry every offset depends on.
//
// So every rune a span cannot legitimately carry is DROPPED here: C0 controls
// (ESC included — no attempt is made to parse and skip "the rest" of a
// sequence, since there is no rest that would be legitimate either), DEL, the
// C1 range that 8-bit terminals read as controls in its own right, and bytes
// that are not valid UTF-8 at all (a raw 0x9b is a CSI introducer). Comp-layer
// sanitizers do this upstream where the text still has meaning and something
// better than deletion is possible; this is the structural backstop that makes
// the guarantee hold for spans that never went through them.
func spanText(s string) string {
	if isPrintableASCII(s) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		i += size
		switch {
		case r == utf8.RuneError && size == 1: // not UTF-8; never re-emitted
		case r < 0x20 || r == 0x7f: // C0 and DEL
		case r >= 0x80 && r <= 0x9f: // C1
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// isPrintableASCII is spanText's fast path: the overwhelmingly common span is
// plain ASCII text that needs no copy at all.
func isPrintableASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < 0x20 || s[i] >= 0x7f {
			return false
		}
	}
	return true
}

func sameRows(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

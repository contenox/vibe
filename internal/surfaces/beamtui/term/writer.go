package term

import (
	"bytes"
	"fmt"
	"io"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/contenox/contenox/internal/surfaces/beamtui/frame"
	"github.com/contenox/contenox/internal/surfaces/beamtui/textwidth"
)

// Escape sequences beam is allowed to emit. No alternate screen, mouse
// tracking, full-screen erase (ED), or absolute cursor addressing (CUP): the
// engine paints inside a region it does not own the boundaries of, so
// blanking is always EL2 plus relative cursor movement, one row at a time.
const (
	seqSyncBegin  = "\x1b[?2026h" // begin synchronized update
	seqSyncEnd    = "\x1b[?2026l" // end synchronized update
	seqCursorHide = "\x1b[?25l"
	seqCursorShow = "\x1b[?25h"
	seqClearLine  = "\x1b[2K" // EL 2: erase the whole current line
	seqClearBelow = "\x1b[0J" // ED 0: erase from the cursor to the end of the screen
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

// painter turns Frames into terminal bytes: the whole commit algorithm with
// none of the terminal syscalls, so every rendering invariant is testable
// headless. Coordinates are relative — the painter never assumes a screen
// row, only "the row the cursor was on when this region started" — which is
// what lets scrollback scroll the terminal naturally.
//
// It assumes DECAWM auto-margin with deferred wrap: writing the last cell of
// a row leaves a pending-wrap flag that only the next printable rune
// resolves, and CR cancels a pending wrap without consuming a row. Every
// erase the painter emits is therefore preceded by CR, so it never erases
// the row a pending wrap parked the cursor on instead of the row the text
// belongs to; cursor moves resolve the pending wrap themselves, so
// moveToRow is safe to emit at any point in a frame.
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

// resize erases the live region at the geometry it was painted with and then
// adopts the new size. The rows were measured at the old width, so every count
// the painter holds is about to become wrong and the diff cache with it — but
// the moment before the size is adopted is the last one in which the region can
// still be located, so that is when it is erased. Disowning it instead leaves
// the composer and the status bar scrolled into permanent history, one copy per
// resize.
//
// On the debounced gesture path this erase is a trivial backstop: disown
// already took the region down at gesture start, while the counts still
// described the screen, and at most one blank suppressed-commit row remains
// (see ANSI.onResizeGesture and ANSI.Commit). The full erase-under-reflow
// reasoning in eraseRegion matters for the sizes that arrive without a gesture
// start — the first size, the one after Suspend, and a hook that lost the race
// to a commit.
//
// The cursor is left at column 0 of the region's origin, which is a blank row
// the next frame paints straight onto: that is why reset()'s p.row = 0 is the
// correct anchor afterwards and no further state has to survive.
func (p *painter) resize(width, height int) {
	if width == p.width && height == p.height {
		return
	}
	p.eraseRegion()
	p.width, p.height = width, height
	p.reset()
}

// disown erases the live region and forgets it, without adopting a size. It is
// the resize-gesture entry point (see ANSI.onResizeGesture): called the moment
// a debounced resize gesture begins, while the painter's row counts still
// describe the screen — the terminal has reflowed at most the gesture's first
// small step by then — so the erase lands exactly where the region is, on
// reflowing and non-reflowing terminals alike. Erasing here instead of at the
// settle is what stops a shrink-then-grow from stranding rewrapped copies of
// the composer, hint and status rows above the origin, where no later erase
// may safely reach (see eraseRegion on why the origin is a hard ceiling).
func (p *painter) disown() {
	p.eraseRegion()
	p.reset()
}

// eraseRegion blanks the live region in place and parks the cursor on its
// origin. Nothing is written before the first frame: there is no region to
// erase, and a resize arriving ahead of the initial commit is the normal case.
//
// Reflow is what shapes this. Live rows are truncated to the width they were
// painted at, so on a terminal that rewraps a narrowing screen — Windows
// Terminal, VTE and iTerm do; xterm does not — a row that filled the old width,
// as the status bar always does, becomes two or more physical rows. Every count
// the painter holds then describes a geometry that no longer exists, in both
// directions at once: prevRows undercounts the region, and p.row undercounts
// how far the caret now sits below the origin. The two directions are not
// symmetric and are not treated alike.
//
// Downward, nothing has to be counted at all. A single ED0 issued at column 0
// of the origin erases from there to the bottom of the screen, covering however
// many rows the rewrap produced, and nothing but the live region is ever below
// the origin, so the sweep can only reach rows the painter owns. That is
// strictly more thorough than blanking prevRows rows and cannot be wrong.
//
// Upward, p.row is used as a floor and deliberately left uncorrected. p.prev
// and the new width would allow an estimate — ceil(textwidth.Width(row)/width)
// physical rows per row above the caret — but whether the terminal reflowed at
// all is not discoverable from here, and the estimate is only right on the
// terminals that do. On one that does not, every estimated row moves the erase
// one row further into the user's committed transcript, which no repaint can
// bring back; the worst a too-short move can do is strand a row of chrome,
// which the next narrowing or a redraw sweeps up. Recoverable beats
// unrecoverable: never move above the origin p.row names.
//
// Rows that reflowed above the caret are therefore the residue this cannot
// reach. Widening never produces any — each live row is its own logical line no
// wider than the width it was painted at, so a growing screen has nothing to
// rejoin. Shrinking is disarmed one step earlier: disown erases the region the
// moment a resize gesture begins, before the drag's later widths can rewrap it,
// so by the time the debounced settle arrives here there is at most one blank
// row to reclaim. What remains for this reasoning is the undebounced residue —
// a size the terminal had already reflowed before the engine heard about it.
func (p *painter) eraseRegion() {
	if !p.painted || p.prevRows == 0 {
		return
	}
	var b bytes.Buffer
	p.moveToRow(&b, 0)
	b.WriteString("\r")
	b.WriteString(seqClearBelow)
	// A failed write only means the stale region stays on screen; the caller
	// is mid-resize and has no way to act on it, and the next commit repaints
	// regardless because reset() sets invalid.
	_, _ = p.out.Write(b.Bytes())
}

// reset forgets the live region entirely: the next commit paints fresh at
// the cursor's current position without reclaiming rows above it. Used
// after Suspend and after a resize (see resize).
func (p *painter) reset() {
	p.prev = nil
	p.prevRows = 0
	p.row = 0
	p.cursor = frame.Cursor{}
	p.painted = false
	p.invalid = true
}

// commit renders one frame: scrollback lines print once into terminal
// history above the live region, then the live region repaints — minimally
// when only its rows changed — inside one synchronized-output bracket with
// the cursor hidden. A frame that changes nothing writes no bytes.
//
// The scrollback path avoids row arithmetic, since printing history raw
// (see renderRaw) makes the physical row count unknowable to the painter:
//
//  1. blank the entire previous live region (EL2 per row).
//  2. print each scrollback line as CR + EL2 + raw text + "\r\n", letting
//     the terminal soft-wrap anything too wide.
//  3. after the last "\r\n" the cursor sits below all printed content —
//     the live region's new origin — so re-anchor there and paint fresh.
func (p *painter) commit(f frame.Frame) error {
	live, cursor := f.Live, f.Cursor
	if p.height > 0 && len(live) > p.height {
		dropped := len(live) - p.height
		live = live[dropped:] // invariant: a too-tall region shows its tail
		// The clamped rows are off-screen, so the caret moves up with them.
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

	// Bookkeeping publishes only once the Write fully succeeds: publishing
	// first would let a cache describe a frame the screen may not show,
	// making an identical retry a no-op that leaves a half-drawn frame.
	if _, err := p.out.Write(b.Bytes()); err != nil {
		// What is on screen is now unknown, so the next commit repaints
		// everything; the anchor rewinds to match the last frame that did
		// land, the only self-consistent state left.
		p.invalid = true
		p.row = anchor
		// Close the sync bracket and show the cursor in case the write died
		// mid-frame, or the terminal stays frozen and blind.
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
// and returns to the first of them, leaving the cursor at column 0, so
// history printed over it cannot expose a fragment of the old composer.
// Only EL2 and relative cursor movement are used, each preceded by CR so a
// pending wrap is resolved rather than clobbered by the erase.
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
// clean line the shell's next prompt lands on. This is Close's guarantee
// that no beam chrome survives an orderly exit.
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
		// An empty region has no row to address, but every frame hides the
		// cursor on the way in, so it must still be shown here.
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
// result to the terminal width. Live rows truncate rather than wrap: the
// region's height is len(Live) by contract, and growing it would corrupt
// every offset the app computed. Live rows are repainted, never copied out
// of history, so truncation costs nothing a redraw does not fix.
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
// terminal whole: no truncation, no wrapping, not even at the terminal's
// width. A line wider than the terminal is soft-wrapped by the terminal
// itself, keeping it one logical line in the selection model, so a paste
// never gains a phantom newline mid-line. The price is that the painter
// cannot know how many physical rows a scrollback line consumed, which is
// why commit's scrollback path never asks.
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
// trusting it: the engine is the last place a byte can become terminal
// behavior, so any ESC smuggled through a span could hijack the screen, and
// a stray \n or \t would break the one-span-row-is-one-terminal-row geometry.
// Every rune a span cannot legitimately carry is dropped: C0 controls
// (including ESC, never parsed as a sequence to skip), DEL, the C1 range,
// and invalid UTF-8. Comp-layer sanitizers do this upstream where better
// recovery than deletion is possible; this is the structural backstop.
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

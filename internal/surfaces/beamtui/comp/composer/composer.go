// Package composer owns beam's input buffer: a pure state machine over text,
// agnostic to terminals and key encodings, behind the bottom-fixed composer
// region. It classifies and packages a Submission but never executes,
// dispatches, or calls the model itself. Every offset is a rune index and
// every column a cell measured through textwidth, never a byte index.
package composer

import (
	"strings"
	"unicode"

	"github.com/contenox/beam/internal/surfaces/beamtui/sanitize"
	"github.com/contenox/beam/internal/surfaces/beamtui/textwidth"
)

// MaxRows is the composer's tallest rendering; taller drafts scroll
// internally with the caret kept visible.
const MaxRows = 6

// prefixWidth is the sigil column every rendered row carries (beam-bar plus
// one space); content wraps to width-prefixWidth and CursorPos accounts for it.
const prefixWidth = 2

// Composer is the input buffer and its caret. The zero value is not usable;
// call New. Not safe for concurrent use: Render caches the caret so
// CursorPos can report it.
type Composer struct {
	// lines is the buffer split on newlines (always >= 1), holding no
	// control runes so cell width is exactly textwidth.Width.
	lines [][]rune
	// line and off are the caret: a line index and rune offset, where
	// off == len(line) means past the last rune.
	line int
	off  int

	// last retains the most recent Submit's text for a failed validation
	// to restore.
	last    string
	hasLast bool

	// history is the recall list, oldest first; histIdx is -1 while
	// editing a draft. stash holds the draft displaced by the first recall.
	history   []string
	histIdx   int
	stash     [][]rune
	stashLine int
	stashOff  int
	hasStash  bool

	// curRow and curCol are the caret in the most recent Render's
	// coordinates.
	curRow int
	curCol int
}

// New returns an empty composer with the caret in the only line.
func New() *Composer {
	return &Composer{
		lines:   [][]rune{{}},
		histIdx: -1,
		curCol:  prefixWidth,
	}
}

// Empty reports whether the buffer holds no characters at all. A buffer of
// only spaces is not empty: Ctrl+C has something to clear even though
// Submit would refuse it.
func (c *Composer) Empty() bool {
	return len(c.lines) == 1 && len(c.lines[0]) == 0
}

// Draft returns the buffer as text, lines joined by "\n". It is the exact
// string the caller seeds $EDITOR with, and SetDraft(Draft()) is an identity.
func (c *Composer) Draft() string {
	if len(c.lines) == 1 {
		return string(c.lines[0])
	}
	parts := make([]string, len(c.lines))
	for i, l := range c.lines {
		parts[i] = string(l)
	}
	return strings.Join(parts, "\n")
}

// SetDraft replaces the whole buffer with s, caret at the end — the return
// leg of the $EDITOR round trip. It never submits or classifies, folds tabs
// to a space and drops other control characters, and detaches from history
// recall like any edit.
func (c *Composer) SetDraft(s string) {
	c.setBuffer(s)
	c.detach()
}

// InsertRune inserts r at the caret. A newline inserts a line break rather
// than submitting; tabs fold to a space, and control and bidi runes are
// ignored.
func (c *Composer) InsertRune(r rune) {
	switch {
	case r == '\n' || r == '\r':
		c.InsertNewline()
		return
	case r == '\t':
		r = ' '
	case unicode.IsControl(r), isBidi(r):
		return
	}
	l := c.lines[c.line]
	out := make([]rune, 0, len(l)+1)
	out = append(out, l[:c.off]...)
	out = append(out, r)
	out = append(out, l[c.off:]...)
	c.lines[c.line] = out
	c.off++
	c.touch()
}

// InsertString inserts s as one edit, whatever it contains. Embedded
// newlines split into buffer lines, and s is never re-read as a submit or a
// slash/shell trigger.
func (c *Composer) InsertString(s string) {
	if s == "" {
		return
	}
	parts := splitText(s)
	l := c.lines[c.line]
	head := append([]rune(nil), l[:c.off]...)
	tail := append([]rune(nil), l[c.off:]...)

	if len(parts) == 1 {
		c.lines[c.line] = concat(head, parts[0], tail)
		c.off = len(head) + len(parts[0])
		c.touch()
		return
	}

	inserted := make([][]rune, len(parts))
	inserted[0] = concat(head, parts[0])
	for i := 1; i < len(parts)-1; i++ {
		inserted[i] = parts[i]
	}
	lastIdx := len(parts) - 1
	inserted[lastIdx] = concat(parts[lastIdx], tail)

	rest := c.lines[c.line+1:]
	next := make([][]rune, 0, len(c.lines)+len(parts)-1)
	next = append(next, c.lines[:c.line]...)
	next = append(next, inserted...)
	next = append(next, rest...)

	c.lines = next
	c.line += lastIdx
	c.off = len(parts[lastIdx])
	c.touch()
}

// InsertNewline breaks the line at the caret; Enter calls Submit instead, so
// the app-shell's key binding is what decides between them.
func (c *Composer) InsertNewline() {
	l := c.lines[c.line]
	head := append([]rune(nil), l[:c.off]...)
	tail := append([]rune(nil), l[c.off:]...)

	next := make([][]rune, 0, len(c.lines)+1)
	next = append(next, c.lines[:c.line]...)
	next = append(next, head, tail)
	next = append(next, c.lines[c.line+1:]...)

	c.lines = next
	c.line++
	c.off = 0
	c.touch()
}

// Backspace deletes the rune before the caret, joining with the previous
// line when the caret is at a line start.
func (c *Composer) Backspace() {
	if c.off > 0 {
		l := c.lines[c.line]
		c.lines[c.line] = concat(l[:c.off-1], l[c.off:])
		c.off--
		c.touch()
		return
	}
	if c.line == 0 {
		return
	}
	prev := c.lines[c.line-1]
	joinAt := len(prev)
	c.lines[c.line-1] = concat(prev, c.lines[c.line])
	c.lines = append(c.lines[:c.line], c.lines[c.line+1:]...)
	c.line--
	c.off = joinAt
	c.touch()
}

// DeleteForward deletes the rune under the caret, pulling up the next line
// when the caret is at a line end.
func (c *Composer) DeleteForward() {
	l := c.lines[c.line]
	if c.off < len(l) {
		c.lines[c.line] = concat(l[:c.off], l[c.off+1:])
		c.touch()
		return
	}
	if c.line == len(c.lines)-1 {
		return
	}
	c.lines[c.line] = concat(l, c.lines[c.line+1])
	c.lines = append(c.lines[:c.line+1], c.lines[c.line+2:]...)
	c.touch()
}

// CursorLeft moves one rune left, wrapping to the end of the previous line.
func (c *Composer) CursorLeft() {
	if c.off > 0 {
		c.off--
		return
	}
	if c.line > 0 {
		c.line--
		c.off = len(c.lines[c.line])
	}
}

// CursorRight moves one rune right, wrapping to the start of the next line.
func (c *Composer) CursorRight() {
	if c.off < len(c.lines[c.line]) {
		c.off++
		return
	}
	if c.line < len(c.lines)-1 {
		c.line++
		c.off = 0
	}
}

// CursorUp moves to the previous buffer line, keeping the caret's cell
// column. On the first line it recalls the previous history entry instead
// (standard shell semantics); with no history it does nothing.
func (c *Composer) CursorUp() {
	if c.line == 0 {
		if c.ShouldRecallUp() {
			c.HistoryUp()
		}
		return
	}
	col := c.cellCol()
	c.line--
	c.off = offsetAtCell(c.lines[c.line], col)
}

// CursorDown moves to the next buffer line, keeping the caret's cell column.
// On the last line it steps forward through history recall instead (and
// past the newest entry, back to the stashed draft); with none in progress
// it does nothing.
func (c *Composer) CursorDown() {
	if c.line == len(c.lines)-1 {
		if c.ShouldRecallDown() {
			c.HistoryDown()
		}
		return
	}
	col := c.cellCol()
	c.line++
	c.off = offsetAtCell(c.lines[c.line], col)
}

// WordLeft jumps to the start of the word before the caret; at a line start
// it steps to the end of the previous line.
func (c *Composer) WordLeft() {
	if c.off == 0 {
		c.CursorLeft()
		return
	}
	c.off = wordLeft(c.lines[c.line], c.off)
}

// WordRight jumps past the end of the word after the caret; at a line end it
// steps to the start of the next line.
func (c *Composer) WordRight() {
	if c.off == len(c.lines[c.line]) {
		c.CursorRight()
		return
	}
	c.off = wordRight(c.lines[c.line], c.off)
}

// DeleteWordBack deletes from the start of the word before the caret to the
// caret. At a line start it behaves like Backspace and joins lines.
func (c *Composer) DeleteWordBack() {
	if c.off == 0 {
		c.Backspace()
		return
	}
	l := c.lines[c.line]
	start := wordLeft(l, c.off)
	c.lines[c.line] = concat(l[:start], l[c.off:])
	c.off = start
	c.touch()
}

// KillToEnd deletes from the caret to the end of the line; at a line end it
// deletes the line break instead, pulling the next line up.
func (c *Composer) KillToEnd() {
	l := c.lines[c.line]
	if c.off == len(l) {
		c.DeleteForward()
		return
	}
	c.lines[c.line] = append([]rune(nil), l[:c.off]...)
	c.touch()
}

// KillToStart deletes from the start of the line to the caret. At a line
// start it does nothing — it never joins lines.
func (c *Composer) KillToStart() {
	if c.off == 0 {
		return
	}
	l := c.lines[c.line]
	c.lines[c.line] = append([]rune(nil), l[c.off:]...)
	c.off = 0
	c.touch()
}

// Home moves the caret to the start of the current line.
func (c *Composer) Home() { c.off = 0 }

// End moves the caret to the end of the current line.
func (c *Composer) End() { c.off = len(c.lines[c.line]) }

// ClearOrPass is Ctrl+C's composer half: a non-empty buffer is cleared and
// the key consumed (true); an empty buffer passes the chord on (false) for
// the app-shell to interrupt or quit.
func (c *Composer) ClearOrPass() bool {
	if c.Empty() {
		return false
	}
	c.clear()
	c.detach()
	return true
}

// Submit classifies and packages the buffer for the caller. A
// whitespace-only buffer is a no-op (ok false, buffer kept); otherwise the
// buffer is cleared and the text retained for RestoreLast.
func (c *Composer) Submit() (Submission, bool) {
	text := c.Draft()
	if strings.TrimSpace(text) == "" {
		return Submission{}, false
	}
	sub := Classify(text)
	c.last = text
	c.hasLast = true
	c.clear()
	c.detach()
	return sub, true
}

// RestoreLast puts the last submitted text back with the caret at the end,
// and reports whether it did. It only restores into an empty buffer and
// consumes the retained text, so later keystrokes win and a second call is
// a no-op.
func (c *Composer) RestoreLast() bool {
	if !c.hasLast || !c.Empty() {
		return false
	}
	c.setBuffer(c.last)
	c.detach()
	c.last, c.hasLast = "", false
	return true
}

// clear empties the buffer and puts the caret in the single remaining line.
func (c *Composer) clear() {
	c.lines = [][]rune{{}}
	c.line, c.off = 0, 0
}

// setBuffer replaces the buffer with s and moves the caret to the end,
// without touching history state — the shared half of SetDraft, history
// recall, and RestoreLast.
func (c *Composer) setBuffer(s string) {
	c.lines = splitText(s)
	c.line = len(c.lines) - 1
	c.off = len(c.lines[c.line])
}

// cellCol is the caret's column within its line, in cells.
func (c *Composer) cellCol() int {
	return textwidth.Width(string(c.lines[c.line][:c.off]))
}

// splitText turns arbitrary text into buffer lines: CRLF and lone CR both
// mean a line break, every other control rune is dropped, and tabs fold to a
// single space. Paste and SetDraft share it, using the shared sanitize
// package since paste is untrusted input deserving the same gate as the
// transcript.
func splitText(s string) [][]rune {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	raw := strings.Split(s, "\n")
	out := make([][]rune, len(raw))
	for i, r := range raw {
		out[i] = []rune(sanitize.Line(r))
	}
	return out
}

// isBidi reports whether r is a Unicode bidi embedding, override or isolate
// control — zero-width format runes that unicode.IsControl misses, checked
// per-rune here since the single-keystroke path has no string to sanitize.
func isBidi(r rune) bool {
	return (r >= 0x202a && r <= 0x202e) || (r >= 0x2066 && r <= 0x2069)
}

// concat joins rune slices into a fresh slice; every mutation builds a new
// line rather than aliasing, so old Draft strings stay valid.
func concat(parts ...[]rune) []rune {
	n := 0
	for _, p := range parts {
		n += len(p)
	}
	out := make([]rune, 0, n)
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

// offsetAtCell returns the rune offset in line whose cell column is closest
// to col without passing it — vertical movement across a line of wide runes
// lands on a rune boundary, never inside one.
func offsetAtCell(line []rune, col int) int {
	w := 0
	for i, r := range line {
		rw := textwidth.Width(string(r))
		if w+rw > col {
			return i
		}
		w += rw
	}
	return len(line)
}

// wordLeft returns the offset at the start of the word before off: skip any
// whitespace, then the word itself.
func wordLeft(line []rune, off int) int {
	i := off
	for i > 0 && unicode.IsSpace(line[i-1]) {
		i--
	}
	for i > 0 && !unicode.IsSpace(line[i-1]) {
		i--
	}
	return i
}

// wordRight returns the offset just past the word after off.
func wordRight(line []rune, off int) int {
	i := off
	for i < len(line) && unicode.IsSpace(line[i]) {
		i++
	}
	for i < len(line) && !unicode.IsSpace(line[i]) {
		i++
	}
	return i
}

// Package composer owns beam's input buffer: the multiline text-editing
// state machine that sits behind the bottom-fixed composer region.
//
// It is a pure state machine over text plus a pure projection of that state
// into frame lines. It knows nothing about terminals, key encodings, the
// keymap, or the engine: the app-shell maps keystrokes onto the semantic
// operations exposed here, and the classified Submission handed back at
// submit tells the caller which consumer owns the line. Per the blueprint's
// ownership ruling (4.11) the composer classifies and packages; it never
// executes a shell line, dispatches a command, or calls the model.
//
// Every offset in this package is a RUNE index and every column is a
// terminal CELL measured through textwidth. The predecessor TUI panicked
// slicing a multibyte string, so nothing here indexes text by byte.
package composer

import (
	"strings"
	"unicode"

	"github.com/contenox/beam/internal/surfaces/beamtui/sanitize"
	"github.com/contenox/beam/internal/surfaces/beamtui/textwidth"
)

// MaxRows is the composer's tallest rendering. The blueprint fixes the
// region at 1–6 lines; taller drafts scroll internally with the caret kept
// visible, so the app-shell's layout arithmetic never has to grow.
const MaxRows = 6

// prefixWidth is the sigil column every rendered row carries: the beam-bar
// plus one space. Content wraps to width-prefixWidth and CursorPos accounts
// for it.
const prefixWidth = 2

// Composer is the input buffer and its caret. The zero value is not usable;
// call New.
//
// It is not safe for concurrent use — Render caches the caret it computed so
// CursorPos can report it — which matches the single-goroutine TUI update
// loop it lives in.
type Composer struct {
	// lines is the buffer, split on newlines, always at least one line.
	// Lines hold no control runes (see sanitize), so a line's cell width is
	// exactly textwidth.Width of it.
	lines [][]rune
	// line and off are the caret: a line index and a rune offset within
	// that line, where off == len(line) is the position after the last
	// rune.
	line int
	off  int

	// last retains the text of the most recent successful Submit so a
	// failed validation can put it back (blueprint 4.11 MVP item 4).
	last    string
	hasLast bool

	// history is the app-provided recall list, oldest first. histIdx is
	// -1 while the user is editing their own draft and otherwise indexes
	// history; stash holds the draft displaced by the first recall.
	history   []string
	histIdx   int
	stash     [][]rune
	stashLine int
	stashOff  int
	hasStash  bool

	// curRow and curCol are the caret in the coordinates of the lines
	// returned by the most recent Render.
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
// only spaces is NOT empty: Ctrl+C still has something to clear, even though
// Submit would refuse it.
func (c *Composer) Empty() bool {
	return len(c.lines) == 1 && len(c.lines[0]) == 0
}

// Draft returns the buffer as text, lines joined by "\n". It is the exact
// string the caller seeds $EDITOR with (blueprint MVP: Ctrl+E composition),
// and SetDraft(Draft()) is an identity.
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

// SetDraft replaces the whole buffer with s and puts the caret at the end.
// It is the return leg of the $EDITOR round trip; it never submits and never
// classifies. Control characters other than newlines are dropped and tabs
// fold to one space, so the buffer stays exactly measurable in cells.
//
// Like any edit it detaches the buffer from history recall.
func (c *Composer) SetDraft(s string) {
	c.setBuffer(s)
	c.detach()
}

// InsertRune inserts r at the caret. A newline rune inserts a line break
// rather than submitting — submission is a decision only the app-shell's
// key mapping makes. Tabs fold to a single space, and control and bidi runes
// are ignored: the buffer holds only runes whose cell width is exactly what
// they draw.
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

// InsertString inserts s literally at the caret: ONE insertion, whatever it
// contains. Embedded newlines split into buffer lines (a trailing newline
// leaves a trailing empty line) and nothing in s is ever re-read as a submit
// or as a slash/shell trigger — that is blueprint MVP item 3, the bracketed
// paste contract, and it is why paste goes through here instead of a loop
// over InsertRune-plus-key-dispatch.
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

// InsertNewline breaks the line at the caret. It is the submit-vs-newline
// distinction's other half: the app-shell binds it to the chord chosen in
// D4 (Ctrl+J / Alt+Enter) while Enter calls Submit.
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
// column as closely as the target line allows.
//
// On the first line it recalls the previous history entry instead (D12 and
// blueprint MVP item 7: standard shell semantics, recall only at the buffer
// edge). With no history it does nothing.
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

// CursorDown moves to the next buffer line, keeping the caret's cell column
// as closely as the target line allows. On the last line it steps forward
// through history recall (and past the newest entry back to the stashed
// draft); with no recall in progress it does nothing.
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

// KillToEnd deletes from the caret to the end of the line; with the caret
// already at a line end it deletes the line break, pulling the next line up
// (readline's Ctrl+K).
func (c *Composer) KillToEnd() {
	l := c.lines[c.line]
	if c.off == len(l) {
		c.DeleteForward()
		return
	}
	c.lines[c.line] = append([]rune(nil), l[:c.off]...)
	c.touch()
}

// KillToStart deletes from the start of the line to the caret (readline's
// Ctrl+U). At a line start it does nothing — it never joins lines, so a
// mistyped chord can't silently eat the line above.
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

// ClearOrPass is the composer's share of the Ctrl+C contract (D3): a
// non-empty buffer is cleared and the key is consumed (true); an empty
// buffer passes the chord on (false) so the app-shell can interrupt the
// in-flight turn or quit.
func (c *Composer) ClearOrPass() bool {
	if c.Empty() {
		return false
	}
	c.clear()
	c.detach()
	return true
}

// Submit classifies and packages the buffer for the caller.
//
// A whitespace-only buffer is a no-op: ok is false and the buffer is kept
// (blueprint MVP item 2). Otherwise the buffer is CLEARED immediately (MVP
// item 4) and the submitted text is retained for RestoreLast.
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

// RestoreLast puts the most recently submitted text back into the buffer
// with the caret at the end — the failed-validation path of blueprint MVP
// item 4 — and reports whether it did.
//
// It restores only into an empty buffer and consumes the retained text, so
// the two ways this could destroy work are both closed: keystrokes the user
// made while the submission was in flight win over the restore, and a second
// call is a no-op rather than a resurrection.
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
// single space so a line's cell width stays exactly measurable. Paste and
// SetDraft share it, so both normalize identically.
//
// The per-line cleaning is the shared sanitize package's, not a second
// implementation of the same rule — a paste is the composer's untrusted
// ingest point (whatever was on the system clipboard), and it deserves the
// same gate the transcript and the overlays use. It is also strictly more
// than the local version managed: bidi controls are Unicode format runes, not
// Cc, so unicode.IsControl walked straight past them and a pasted override
// would have reordered the prompt on screen.
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

// isBidi reports whether r is one of the Unicode bidi embedding, override or
// isolate controls. They are zero-width and reorder everything after them, so
// a buffer holding one draws as something other than what it says — and they
// are format runes, not Cc, which is why unicode.IsControl does not catch
// them. The same set the sanitize package removes, checked per-rune here
// because the single-keystroke path has no string to hand it.
func isBidi(r rune) bool {
	return (r >= 0x202a && r <= 0x202e) || (r >= 0x2066 && r <= 0x2069)
}

// concat joins rune slices into a fresh slice; every buffer mutation builds
// a new line rather than aliasing, so a caller holding an old Draft string
// is never surprised.
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

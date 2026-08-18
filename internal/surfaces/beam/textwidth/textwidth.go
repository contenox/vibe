// Package textwidth centralizes rune-safe terminal cell-width math for
// beam: every width measurement, truncation, pad, and wrap goes through
// here instead of ad hoc len() arithmetic.
//
// Widths are terminal cells (East-Asian wide runes count 2). Because frame
// spans carry no escape codes, Width(line.Text()) is a line's rendered width.
package textwidth

import (
	"strings"

	"github.com/mattn/go-runewidth"
)

// cond pins East-Asian-Ambiguous runes to narrow regardless of locale.
// go-runewidth's package-level functions consult RUNEWIDTH_EASTASIAN and a
// CJK LANG at init, which would make width math (and every golden file)
// depend on the machine running the tests. Beam trades locale adaptation
// for determinism: engine and components share this one condition.
var cond = &runewidth.Condition{EastAsianWidth: false}

// Width returns the cell width of s.
func Width(s string) int { return cond.StringWidth(s) }

// Truncate cuts s to at most w cells, appending tail (whose width counts
// toward w) when anything was removed. Never splits a rune.
func Truncate(s string, w int, tail string) string { return cond.Truncate(s, w, tail) }

// Pad right-pads s with spaces to exactly w cells; wider input is returned
// unchanged.
func Pad(s string, w int) string { return cond.FillRight(s, w) }

// Wrap wraps s to at most w cells per line, breaking after spaces where
// possible and hard-breaking only words wider than w. Existing newlines are
// respected, and every rune of s appears in the output exactly once, in
// order — Wrap only ever inserts breaks, leaving a space at the end of the
// earlier line, a property the composer's caret math counts on.
//
// A non-positive w still splits on newlines and does no other wrapping: a
// returned element is a terminal row headed straight for a frame.Span, and
// a span holding a newline breaks frame.Line's one-row contract, so a
// degenerate width must never return the input whole.
//
// go-runewidth's own Wrap is not used: its word-boundary detection breaks
// after any non-ASCII rune, hard-splitting words mid-run.
func Wrap(s string, w int) []string {
	if w <= 0 {
		return strings.Split(s, "\n")
	}
	var out []string
	for _, para := range strings.Split(s, "\n") {
		out = append(out, wrapLine(para, w)...)
	}
	return out
}

func wrapLine(s string, w int) []string {
	if Width(s) <= w {
		return []string{s}
	}
	var lines []string
	var line []rune
	lineW := 0
	lastSpace := -1 // index in line of the last space, a legal break point
	for _, r := range s {
		rw := cond.RuneWidth(r)
		for lineW+rw > w && len(line) > 0 {
			if lastSpace >= 0 {
				lines = append(lines, string(line[:lastSpace+1]))
				rest := append([]rune(nil), line[lastSpace+1:]...)
				line = rest
				lastSpace = -1
			} else {
				lines = append(lines, string(line))
				line = line[:0]
			}
			lineW = Width(string(line))
		}
		line = append(line, r)
		lineW += rw
		if r == ' ' {
			lastSpace = len(line) - 1
		}
	}
	return append(lines, string(line))
}

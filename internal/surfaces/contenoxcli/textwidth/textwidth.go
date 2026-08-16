// Package textwidth centralizes rune-safe terminal cell-width math for beam.
// Widths are terminal cells, with East-Asian wide runes counting 2.
package textwidth

import (
	"strings"

	"github.com/mattn/go-runewidth"
)

// cond pins East-Asian-Ambiguous runes to narrow regardless of locale, so width
// math does not depend on the machine running the tests.
var cond = &runewidth.Condition{EastAsianWidth: false}

// Width returns the cell width of s.
func Width(s string) int { return cond.StringWidth(s) }

// Truncate cuts s to at most w cells, appending tail (whose width counts toward
// w) when anything was removed. It never splits a rune.
func Truncate(s string, w int, tail string) string { return cond.Truncate(s, w, tail) }

// Pad right-pads s with spaces to exactly w cells; wider input is returned
// unchanged.
func Pad(s string, w int) string { return cond.FillRight(s, w) }

// Wrap wraps s to at most w cells per line, breaking after spaces where possible
// and hard-breaking only words wider than w. Every rune of s appears in the
// output exactly once and in order; Wrap only ever inserts breaks. A
// non-positive w still splits on newlines and does no other wrapping.
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

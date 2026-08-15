// Package brand renders contenox's identity device for plain-writer surfaces:
// the logo-mark as block art beside the wordmark, over the brand-mint gutter
// bar. It is the header `contenox serve` prints before its status screen.
//
// The mark and its colour ramp are the same ones beam's TUI draws, kept
// faithful on purpose — a person who has seen one should recognise the other.
// The difference is the machinery: beam composes styled frame.Lines for a
// full-screen renderer, while this writes strings to an io.Writer and decides
// colour from terminal capability. That is why the art is duplicated rather
// than imported: beam is a separate module and the art lives in its internal/
// tree, so there is nothing to import even if the rendering models matched.
//
// Everything here is a pure function of (options) → string. No terminal reads:
// the caller decides Colour and ASCII, so tests pin exact bytes.
package brand

import (
	"io"
	"strings"
)

// Wordmark and tagline are fixed brand copy: lowercase "contenox", em-dash in
// unicode and a plain hyphen in ASCII — do not paraphrase either string.
const (
	wordmark       = "contenox"
	taglineUnicode = " — an agent server"
	taglineASCII   = " - an agent server"
)

// The beam-bar: the vertical stroke the device is built around.
const (
	gutterUnicode = "▌"
	gutterASCII   = "|"
	gutterGap     = "  "
)

// The brand ramp, lightest rung first, as ANSI 256-colour foregrounds. 85/78/29
// is beam's ladder; 78 is the brand's fixed mid stop in both light and dark
// terminals, so the mark does not need to know the theme.
const (
	ramp1 = "\x1b[38;5;85m"
	ramp2 = "\x1b[38;5;78m"
	ramp3 = "\x1b[38;5;29m"
	dim   = "\x1b[2m"
	reset = "\x1b[0m"
)

// span is one coloured run inside an art row. Rows are span lists because the
// mark's blades interleave within a single row.
type span struct {
	colour string // "" means unstyled
	text   string
}

// artUnicode is the logo-mark rasterized from website/public/logo-mark.svg:
// three blades — quarter-arc outer edge, straight inner edge, mitered ends —
// pinwheel-arranged around a square core, opening right. Rendered with
// half-blocks (one text row = two pixel rows), lightest at top. Keep any
// change faithful to the source mark.
var artUnicode = [5][]span{
	{{"", "    "}, {ramp1, "▄▄███"}},
	{{"", " "}, {ramp2, "▄▄"}, {"", " "}, {ramp1, "▀▀▀▀▀"}},
	{{ramp2, "▄██"}},
	{{ramp2, "███"}, {"", " "}, {ramp3, "▄▄▄▄▄"}},
	{{"", "    "}, {ramp3, "▀▀███"}},
}

// artASCII suggests the same swirl in characters a legacy console can draw:
// diagonals stand in for the arc, gaps stay. The mark is symmetric about its
// middle row, so ASCII reads as the same open C rather than a lopsided hook.
var artASCII = [5][]span{
	{{"", "    "}, {ramp1, ",==="}},
	{{"", "   "}, {ramp2, "/"}, {"", " "}, {ramp1, "~~~"}},
	{{"", "  "}, {ramp2, "|"}},
	{{"", "   "}, {ramp2, "\\"}, {"", " "}, {ramp3, "___"}},
	{{"", "    "}, {ramp3, "`==="}},
}

// wordmarkRow is the art row the wordmark hangs beside: the mark's vertical
// middle, so the two read as one device rather than stacked decorations.
const wordmarkRow = 2

// Options selects how the header renders. The zero value is plain ASCII with
// no escapes, which is what a redirected stdout should get.
type Options struct {
	// Colour emits ANSI foreground escapes. Callers set it from a TTY check.
	Colour bool
	// ASCII swaps the block art and em-dash for characters a legacy console
	// can draw.
	ASCII bool
}

// Header returns the identity block: five art rows, the wordmark beside the
// mark's middle row, and the tagline under it. It ends with a newline and no
// blank line, so the caller controls the spacing to whatever follows.
func Header(opts Options) string {
	art := artUnicode
	tagline := taglineUnicode
	gutter := gutterUnicode
	if opts.ASCII {
		art = artASCII
		tagline = taglineASCII
		gutter = gutterASCII
	}

	// The art column is padded to a fixed width so the wordmark lands in the
	// same column on every row, including rows the art leaves short.
	artWidth := 0
	for _, row := range art {
		w := 0
		for _, s := range row {
			w += len([]rune(s.text))
		}
		if w > artWidth {
			artWidth = w
		}
	}

	var b strings.Builder
	for i, row := range art {
		b.WriteString(colourise(gutter, ramp2, opts.Colour))
		b.WriteString(gutterGap)

		width := 0
		for _, s := range row {
			b.WriteString(colourise(s.text, s.colour, opts.Colour))
			width += len([]rune(s.text))
		}
		b.WriteString(strings.Repeat(" ", artWidth-width))

		// The wordmark and its tagline share the mark's middle row, joined by
		// the brand's dash: one identity line beside the device, rather than a
		// second block of text stacked under it.
		if i == wordmarkRow {
			b.WriteString("  ")
			b.WriteString(colourise(wordmark, ramp1, opts.Colour))
			b.WriteString(colourise(tagline, dim, opts.Colour))
		}
		b.WriteString("\n")
	}
	return b.String()
}

// WriteHeader writes [Header] to w, ignoring the write error the way every
// other cosmetic print does: a header that cannot be printed must not fail the
// command that was about to do real work.
func WriteHeader(w io.Writer, opts Options) {
	_, _ = io.WriteString(w, Header(opts))
}

// colourise wraps s in an escape when colour is on and the style is non-empty.
func colourise(s, colour string, on bool) string {
	if !on || colour == "" || s == "" {
		return s
	}
	return colour + s + reset
}

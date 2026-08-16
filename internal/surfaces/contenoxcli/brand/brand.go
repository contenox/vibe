// Package brand renders contenox's identity device for plain-writer surfaces:
// the logo-mark as block art beside the wordmark. Everything here is a pure
// function of options to string; the caller decides Colour and ASCII.
package brand

import (
	"io"
	"strings"
)

// Fixed brand copy: do not paraphrase either string.
const (
	wordmark       = "contenox"
	taglineUnicode = " — an agent server"
	taglineASCII   = " - an agent server"
)

const (
	gutterUnicode = "▌"
	gutterASCII   = "|"
	gutterGap     = "  "
)

// The brand ramp, lightest rung first, as ANSI 256-colour foregrounds.
const (
	ramp1 = "\x1b[38;5;85m"
	ramp2 = "\x1b[38;5;78m"
	ramp3 = "\x1b[38;5;29m"
	dim   = "\x1b[2m"
	reset = "\x1b[0m"
)

type span struct {
	colour string // "" means unstyled
	text   string
}

// artUnicode is the logo-mark rasterized from website/public/logo-mark.svg,
// rendered with half-blocks. Keep any change faithful to the source mark.
var artUnicode = [5][]span{
	{{"", "    "}, {ramp1, "▄▄███"}},
	{{"", " "}, {ramp2, "▄▄"}, {"", " "}, {ramp1, "▀▀▀▀▀"}},
	{{ramp2, "▄██"}},
	{{ramp2, "███"}, {"", " "}, {ramp3, "▄▄▄▄▄"}},
	{{"", "    "}, {ramp3, "▀▀███"}},
}

// artASCII suggests the same swirl in characters a legacy console can draw.
var artASCII = [5][]span{
	{{"", "    "}, {ramp1, ",==="}},
	{{"", "   "}, {ramp2, "/"}, {"", " "}, {ramp1, "~~~"}},
	{{"", "  "}, {ramp2, "|"}},
	{{"", "   "}, {ramp2, "\\"}, {"", " "}, {ramp3, "___"}},
	{{"", "    "}, {ramp3, "`==="}},
}

// wordmarkRow is the art row the wordmark hangs beside.
const wordmarkRow = 2

// Options selects how the header renders. The zero value is plain ASCII with no
// escapes.
type Options struct {
	// Colour emits ANSI foreground escapes.
	Colour bool
	// ASCII swaps the block art and em-dash for characters a legacy console
	// can draw.
	ASCII bool
}

// Header returns the identity block: five art rows, the wordmark beside the
// mark's middle row, and the tagline under it. It ends with a newline.
func Header(opts Options) string {
	art := artUnicode
	tagline := taglineUnicode
	gutter := gutterUnicode
	if opts.ASCII {
		art = artASCII
		tagline = taglineASCII
		gutter = gutterASCII
	}

	// Padded to a fixed width so the wordmark lands in the same column.
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

		if i == wordmarkRow {
			b.WriteString("  ")
			b.WriteString(colourise(wordmark, ramp1, opts.Colour))
			b.WriteString(colourise(tagline, dim, opts.Colour))
		}
		b.WriteString("\n")
	}
	return b.String()
}

// WriteHeader writes [Header] to w, ignoring the write error.
func WriteHeader(w io.Writer, opts Options) {
	_, _ = io.WriteString(w, Header(opts))
}

func colourise(s, colour string, on bool) string {
	if !on || colour == "" || s == "" {
		return s
	}
	return colour + s + reset
}

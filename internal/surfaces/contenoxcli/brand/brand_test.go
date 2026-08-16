package brand

import (
	"strings"
	"testing"
)

// The header is printed above a status screen, so its shape is load-bearing:
// five art rows, every row starting with the beam-bar, and the wordmark on the
// mark's middle row rather than floating above or below it.
func TestUnit_Brand_HeaderShape(t *testing.T) {
	out := Header(Options{})
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) != 5 {
		t.Fatalf("want 5 art rows, got %d:\n%s", len(lines), out)
	}
	for i, l := range lines {
		if !strings.HasPrefix(l, gutterUnicode) {
			t.Fatalf("row %d does not start with the beam-bar: %q", i, l)
		}
	}
	if !strings.Contains(lines[wordmarkRow], wordmark) {
		t.Fatalf("wordmark belongs on row %d, got %q", wordmarkRow, lines[wordmarkRow])
	}
	if !strings.Contains(lines[wordmarkRow], "an agent server") {
		t.Fatalf("the tagline shares the wordmark's row, got %q", lines[wordmarkRow])
	}
}

// The art is padded into a rectangle so the identity line hangs off it at a
// fixed column even though the mark's rows are ragged.
func TestUnit_Brand_ArtIsPaddedToARectangle(t *testing.T) {
	for _, ascii := range []bool{false, true} {
		lines := strings.Split(strings.TrimRight(Header(Options{ASCII: ascii}), "\n"), "\n")

		want := -1
		for i, l := range lines {
			if i == wordmarkRow {
				continue // carries the identity line past the art block
			}
			w := len([]rune(l))
			if want < 0 {
				want = w
			}
			if w != want {
				t.Fatalf("ascii=%v: art row %d is %d cells, want %d — the block is ragged", ascii, i, w, want)
			}
		}
		// The identity line starts two cells past the art block, on every mode.
		if got := column(lines[wordmarkRow], wordmark); got != want+2 {
			t.Fatalf("ascii=%v: wordmark at column %d, want %d (art block %d + gap 2)", ascii, got, want+2, want)
		}
	}
}

// column reports where sub starts in display cells, not bytes: the block
// glyphs are three bytes each, so a byte offset would report two visually
// aligned rows as misaligned.
func column(line, sub string) int {
	i := strings.Index(line, sub)
	if i < 0 {
		return -1
	}
	return len([]rune(line[:i]))
}

// A redirected stdout gets the zero value, which must contain no escapes at
// all — a status screen piped into a file or a log should stay readable.
func TestUnit_Brand_PlainHasNoEscapes(t *testing.T) {
	if strings.Contains(Header(Options{}), "\x1b") {
		t.Fatal("the zero value must emit no ANSI escapes")
	}
	if !strings.Contains(Header(Options{Colour: true}), "\x1b") {
		t.Fatal("Colour must emit ANSI escapes")
	}
}

// ASCII mode exists for consoles that cannot draw half-blocks, so it must not
// leak one — including through the tagline's em-dash.
func TestUnit_Brand_ASCIIIsASCIIOnly(t *testing.T) {
	out := Header(Options{ASCII: true, Colour: true})
	for _, r := range out {
		if r > 127 && r != '\x1b' {
			t.Fatalf("non-ASCII rune %q in ASCII header:\n%s", r, out)
		}
	}
}

// Colour must wrap runs without disturbing the glyphs, so stripping the escapes
// has to reproduce the plain rendering exactly.
func TestUnit_Brand_ColourOnlyAddsEscapes(t *testing.T) {
	for _, ascii := range []bool{false, true} {
		plain := Header(Options{ASCII: ascii})
		coloured := Header(Options{ASCII: ascii, Colour: true})
		stripped := coloured
		for _, esc := range []string{ramp1, ramp2, ramp3, dim, reset} {
			stripped = strings.ReplaceAll(stripped, esc, "")
		}
		if stripped != plain {
			t.Fatalf("ascii=%v: colour changed the glyphs\nplain:\n%s\nstripped:\n%s", ascii, plain, stripped)
		}
	}
}

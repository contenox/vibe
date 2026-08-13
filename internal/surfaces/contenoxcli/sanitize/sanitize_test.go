package sanitize

import (
	"strings"
	"testing"

	"github.com/contenox/contenox/internal/surfaces/contenoxcli/textwidth"
)

// TestUnit_LineStripsEverythingDangerous is the ingest contract in one table:
// whatever a peer sends, what comes back is drawable as literal cells.
func TestUnit_LineStripsEverythingDangerous(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"clean", "an ordinary title", "an ordinary title"},
		{"utf8 survives", "日本語 ✓ émoji 🙂", "日本語 ✓ émoji 🙂"},
		{"csi", "before\x1b[2Jafter", "beforeafter"},
		{"csi with parameters", "a\x1b[38;5;196mb", "ab"},
		{"sgr reset", "\x1b[0mplain\x1b[0m", "plain"},
		{"osc bel", "\x1b]0;window title\x07after", "after"},
		{"osc st", "\x1b]8;;http://evil\x1b\\link", "link"},
		{"dcs", "a\x1bPq...\x1b\\b", "ab"},
		{"charset select", "a\x1b(0b", "ab"},
		{"two byte escape", "a\x1bcb", "ab"},
		{"bare escape at the end", "text\x1b", "text"},
		{"cursor motion", "a\x1b[2Kb\x1b[1;1Hc", "abc"},
		{"tab folds to one space", "a\tb", "a b"},
		{"newline is removed", "my\nsession", "mysession"},
		{"carriage return", "10%\r20%", "10%20%"},
		{"nul bel backspace del", "a\x00\x07\x08b\x7f", "ab"},
		{"bidi override", "+‮drawkcab", "+drawkcab"},
		{"bidi isolates", "a⁦b⁩c", "abc"},
		{"every bidi control", "x‪‫‬‭‮⁦⁧⁨⁩y", "xy"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := Line(c.in); got != c.want {
				t.Fatalf("Line(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

// TestUnit_LinesKeepsStructure: "\n" and "\t" are the two characters a caller
// still needs, because only the caller knows where its lines start.
func TestUnit_LinesKeepsStructure(t *testing.T) {
	cases := []struct{ in, want string }{
		{"one\ntwo", "one\ntwo"},
		{"a\tb", "a\tb"},
		{"one\x1b[2J\ntwo", "one\ntwo"},
		{"keep\r\nthe newline", "keep\nthe newline"},
		{"‮flip\nplain", "flip\nplain"},
	}
	for _, c := range cases {
		if got := Lines(c.in); got != c.want {
			t.Fatalf("Lines(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestUnit_OutputIsAlwaysSpanSafe is the property the whole package exists
// for: no output of Line may carry a rune a span is forbidden to hold, and
// Lines may carry nothing but "\n" and "\t" on top of that.
func TestUnit_OutputIsAlwaysSpanSafe(t *testing.T) {
	inputs := []string{
		"", "plain", "日本語", "\x1b[2J\x1b]0;t\x07\x1bPx\x1b\\",
		"mixed \x1b[31mred\x1b[0m and \ttab\nand newline\r\n",
		"‮⁦nested⁩‬",
		"\x00\x01\x02\x03\x04\x05\x06\x07\x08\x09\x0a\x0b\x0c\x0d\x0e\x0f\x7f",
		strings.Repeat("\x1b[1m", 40) + "tail",
	}
	for _, in := range inputs {
		for _, r := range Line(in) {
			if r < 0x20 || r == 0x7f || isBidi(r) {
				t.Fatalf("Line(%q) emitted %U", in, r)
			}
		}
		for _, r := range Lines(in) {
			if r == '\n' || r == '\t' {
				continue
			}
			if r < 0x20 || r == 0x7f || isBidi(r) {
				t.Fatalf("Lines(%q) emitted %U", in, r)
			}
		}
	}
}

// TestUnit_CleanInputIsReturnedUnchanged pins the fast path: the common case
// costs a scan and no allocation, so sanitizing at every ingest point is not
// a reason to skip one.
func TestUnit_CleanInputIsReturnedUnchanged(t *testing.T) {
	for _, s := range []string{"", "plain ascii", "日本語のテキスト", "→ an arrow"} {
		if got := Line(s); got != s {
			t.Fatalf("Line(%q) = %q, want it unchanged", s, got)
		}
		if got := Lines(s + "\n\tmore"); got != s+"\n\tmore" {
			t.Fatalf("Lines dropped structure from %q: %q", s, got)
		}
	}
	// An arrow shares its lead byte with the bidi block; the cheap prefilter
	// must not mistake it for one, and must not stop scanning at it either.
	if got := Line("→\x1b[2Jx"); got != "→x" {
		t.Fatalf("Line = %q, want %q", got, "→x")
	}
}

func TestUnit_ExpandTabs(t *testing.T) {
	cases := []struct {
		in   string
		stop int
		want string
	}{
		{"no tabs", 8, "no tabs"},
		{"\tfmt.Println", 8, "        fmt.Println"},
		{"a\tb", 8, "a       b"},
		{"ab\tc", 8, "ab      c"},
		{"1234567\tx", 8, "1234567 x"},
		{"12345678\tx", 8, "12345678        x"},
		{"\t\ta", 8, "                a"},
		{"a\tb", 4, "a   b"},
		{"a\tb", 0, "a       b"}, // non-positive falls back to the default
		// Cell-aware: a wide rune advances the column by two.
		{"日\tx", 8, "日      x"},
	}
	for _, c := range cases {
		if got := ExpandTabs(c.in, c.stop); got != c.want {
			t.Fatalf("ExpandTabs(%q, %d) = %q, want %q", c.in, c.stop, got, c.want)
		}
	}
}

// TestUnit_ExpandTabsLandsOnStops is the alignment property tab expansion
// exists for: whatever precedes a tab, the next column is a multiple of the
// stop — which is what keeps a column of shell output or a diff hunk aligned.
func TestUnit_ExpandTabsLandsOnStops(t *testing.T) {
	prefixes := []string{"", "a", "ab", "abc", "日", "日本", "🙂", "1234567", "12345678"}
	for _, p := range prefixes {
		for _, stop := range []int{2, 4, 8} {
			got := ExpandTabs(p+"\tX", stop)
			col := textwidth.Width(got[:strings.Index(got, "X")])
			if col%stop != 0 {
				t.Fatalf("ExpandTabs(%q, %d) = %q: X sits at column %d, not a stop", p, stop, got, col)
			}
		}
	}
}

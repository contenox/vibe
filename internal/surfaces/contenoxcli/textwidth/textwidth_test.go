package textwidth

import (
	"strings"
	"testing"
)

func TestUnit_WidthCountsCells(t *testing.T) {
	cases := []struct {
		in   string
		want int
	}{
		{"", 0},
		{"abc", 3},
		{"héllo", 5},
		{"日本", 4}, // wide runes are 2 cells
		{"a日b", 4},
		{"👍", 2},
	}
	for _, c := range cases {
		if got := Width(c.in); got != c.want {
			t.Errorf("Width(%q) = %d, want %d", c.in, got, c.want)
		}
	}
}

func TestUnit_TruncateIsRuneSafe(t *testing.T) {
	got := Truncate("日本語テキスト", 7, "…")
	if w := Width(got); w > 7 {
		t.Fatalf("Truncate width = %d, want <= 7 (%q)", w, got)
	}
	if got := Truncate("short", 10, "…"); got != "short" {
		t.Fatalf("Truncate should not touch fitting input, got %q", got)
	}
}

func TestUnit_WrapNeverExceedsWidth(t *testing.T) {
	for _, line := range Wrap("the quick 日本語 fox jumps over the lazy dog", 10) {
		if w := Width(line); w > 10 {
			t.Fatalf("wrapped line %q is %d cells, want <= 10", line, w)
		}
	}
}

func TestUnit_WrapBreaksAtWordBoundaries(t *testing.T) {
	cases := []struct {
		in   string
		w    int
		want []string
	}{
		{"the quick brown fox jumps", 10, []string{"the quick ", "brown fox ", "jumps"}},
		// The em-dash case go-runewidth got wrong: no mid-word split after
		// a non-ASCII rune.
		{"of 200 — approving accepts", 12, []string{"of 200 — ", "approving ", "accepts"}},
		{"supercalifragilistic wrap", 10, []string{"supercalif", "ragilistic", " wrap"}},
		{"fits", 10, []string{"fits"}},
		{"a\nb c", 3, []string{"a", "b c"}},
	}
	for _, c := range cases {
		got := Wrap(c.in, c.w)
		if len(got) != len(c.want) {
			t.Errorf("Wrap(%q,%d) = %q, want %q", c.in, c.w, got, c.want)
			continue
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("Wrap(%q,%d)[%d] = %q, want %q", c.in, c.w, i, got[i], c.want[i])
			}
		}
	}
}

// TestUnit_WrapSplitsNewlinesAtEveryWidth pins that no width, degenerate
// ones included, may hand a caller a string with a newline in it.
func TestUnit_WrapSplitsNewlinesAtEveryWidth(t *testing.T) {
	for _, in := range []string{"a\nb", "\n", "one\ntwo\nthree", "", "no newline"} {
		for _, w := range []int{-5, -1, 0, 1, 2, 40} {
			got := Wrap(in, w)
			if len(got) == 0 {
				t.Fatalf("Wrap(%q,%d) returned nothing", in, w)
			}
			for i, line := range got {
				if strings.ContainsRune(line, '\n') {
					t.Fatalf("Wrap(%q,%d)[%d] = %q holds a newline", in, w, i, line)
				}
			}
		}
	}
	// At a degenerate width the only thing that happens is the newline split.
	if got := Wrap("a long line that would wrap", 0); len(got) != 1 || got[0] != "a long line that would wrap" {
		t.Fatalf("Wrap(_, 0) = %q, want the line unwrapped", got)
	}
}

func TestUnit_WrapInsertsBreaksOnly(t *testing.T) {
	inputs := []string{
		"the quick brown fox jumps over the lazy dog",
		"⚠ diff truncated: showing 120 of 200 — approving accepts changes you have not seen.",
		"日本語のテキスト and mixed ascii words",
		"a          b",
		"trailing space ",
		"nospaceatallinthisverylongtoken",
	}
	for _, in := range inputs {
		for w := 3; w <= 40; w++ {
			joined := strings.Join(Wrap(in, w), "")
			if joined != in {
				t.Fatalf("Wrap(%q,%d) altered runes: joined %q", in, w, joined)
			}
		}
	}
}

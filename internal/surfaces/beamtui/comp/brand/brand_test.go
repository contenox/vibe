package brand

import (
	"fmt"
	"strings"
	"testing"

	"github.com/contenox/contenox/internal/surfaces/beamtui/frame"
	"github.com/contenox/contenox/internal/surfaces/beamtui/testkit"
	"github.com/contenox/contenox/internal/surfaces/beamtui/textwidth"
)

// goldenWidths is the resize matrix: narrow (compact layout, 60 sits below CompactWidth on purpose), default, and wide.
var goldenWidths = []int{60, 80, 120}

func sampleInfo(ascii bool) Info {
	return Info{
		ASCII:    ascii,
		Model:    "qwen3-coder:30b",
		Provider: "ollama",
		Session:  "sess-7f3a",
	}
}

// TestUnit_WelcomeGoldens pins the header's exact shape at every width and variant.
func TestUnit_WelcomeGoldens(t *testing.T) {
	variants := []struct {
		name string
		info Info
	}{
		{"unicode", Info{}},
		{"unicode_info", sampleInfo(false)},
		{"ascii", Info{ASCII: true}},
		{"ascii_info", sampleInfo(true)},
	}

	for _, v := range variants {
		for _, w := range goldenWidths {
			name := fmt.Sprintf("welcome_%s_w%d", v.name, w)
			t.Run(name, func(t *testing.T) {
				testkit.Golden(t, name, testkit.EncodeLines(Welcome(w, v.info)))
			})
		}
	}
}

// TestUnit_WelcomeNeverExceedsWidth pins that no line spills a cell, even with overlong caller data.
func TestUnit_WelcomeNeverExceedsWidth(t *testing.T) {
	infos := []struct {
		name string
		info Info
	}{
		{"bare", Info{}},
		{"bare_ascii", Info{ASCII: true}},
		{"info", sampleInfo(false)},
		{"info_ascii", sampleInfo(true)},
		{"overlong", Info{
			Model:    strings.Repeat("very-long-model-", 12),
			Provider: strings.Repeat("provider-", 12),
			Session:  strings.Repeat("s", 200),
		}},
		{"overlong_ascii", Info{
			ASCII:    true,
			Model:    strings.Repeat("very-long-model-", 12),
			Provider: strings.Repeat("provider-", 12),
			Session:  strings.Repeat("s", 200),
		}},
	}

	for _, c := range infos {
		t.Run(c.name, func(t *testing.T) {
			for w := 20; w <= 140; w++ {
				for i, l := range Welcome(w, c.info) {
					if got := textwidth.Width(l.Text()); got > w {
						t.Fatalf("width %d line %d: %d cells > width (%q)", w, i, got, l.Text())
					}
				}
			}
		})
	}
}

// TestUnit_WelcomeEndsWithSeparator pins that the header's last line is always an empty separator.
func TestUnit_WelcomeEndsWithSeparator(t *testing.T) {
	for _, ascii := range []bool{false, true} {
		for w := 20; w <= 140; w++ {
			lines := Welcome(w, Info{ASCII: ascii})
			last := lines[len(lines)-1]
			if last.Text() != "" {
				t.Fatalf("ascii=%v width %d: last line = %q, want empty separator", ascii, w, last.Text())
			}
			if len(lines) < 3 {
				t.Fatalf("ascii=%v width %d: %d lines, want at least two rows plus separator", ascii, w, len(lines))
			}
		}
	}
}

// TestUnit_WordmarkCopyIsExact pins the fixed brand copy: lowercase contenox, em-dash in unicode, hyphen in ASCII.
func TestUnit_WordmarkCopyIsExact(t *testing.T) {
	cases := []struct {
		ascii bool
		want  string
	}{
		{false, "contenox — open coding harness"},
		{true, "contenox - open coding harness"},
	}
	for _, c := range cases {
		for _, w := range []int{60, 80, 120} {
			joined := lineTexts(Welcome(w, Info{ASCII: c.ascii}))
			if !strings.Contains(joined, c.want) {
				t.Fatalf("ascii=%v width %d: wordmark line %q missing from:\n%s", c.ascii, w, c.want, joined)
			}
		}
	}
	// The em-dash must never leak into the ASCII variant.
	if got := lineTexts(Welcome(80, Info{ASCII: true})); strings.ContainsAny(got, "—·…▌█◢◥") {
		t.Fatalf("ASCII welcome contains non-ASCII runes:\n%s", got)
	}
}

// TestUnit_WelcomeUsesOnlyClosedStyleIDs enforces frame's closed StyleID set, ramp roles included.
func TestUnit_WelcomeUsesOnlyClosedStyleIDs(t *testing.T) {
	known := map[frame.StyleID]bool{}
	for _, id := range frame.All() {
		known[id] = true
	}
	for _, ascii := range []bool{false, true} {
		for _, w := range []int{20, 60, 80, 120} {
			for _, l := range Welcome(w, sampleInfo(ascii)) {
				for _, s := range l {
					if !known[s.Style] {
						t.Fatalf("ascii=%v width %d: span %q uses unknown StyleID %q", ascii, w, s.Text, s.Style)
					}
				}
			}
		}
		for _, s := range StatusSegment(ascii) {
			if !known[s.Style] {
				t.Fatalf("ascii=%v: status span %q uses unknown StyleID %q", ascii, s.Text, s.Style)
			}
		}
	}

	// The header must use all three ramp stops, or the roles are dead weight.
	used := map[frame.StyleID]bool{}
	for _, l := range Welcome(80, Info{}) {
		for _, s := range l {
			used[s.Style] = true
		}
	}
	for _, id := range []frame.StyleID{frame.StyleBrandRamp1, frame.StyleBrandRamp2, frame.StyleBrandRamp3, frame.StyleBrand} {
		if !used[id] {
			t.Fatalf("full welcome header never uses %q", id)
		}
	}
}

// TestUnit_StatusSegment pins the persistent identity's spans and width exactly.
func TestUnit_StatusSegment(t *testing.T) {
	cases := []struct {
		ascii bool
		want  frame.Line
		text  string
	}{
		{
			false,
			frame.L(
				frame.S(frame.StyleBrand, "▌"),
				frame.S(frame.StyleNone, " "),
				frame.S(frame.StyleMuted, "contenox"),
			),
			"▌ contenox",
		},
		{
			true,
			frame.L(
				frame.S(frame.StyleBrand, "|"),
				frame.S(frame.StyleNone, " "),
				frame.S(frame.StyleMuted, "contenox"),
			),
			"| contenox",
		},
	}

	for _, c := range cases {
		got := StatusSegment(c.ascii)
		if len(got) != len(c.want) {
			t.Fatalf("ascii=%v: %d spans, want %d (%v)", c.ascii, len(got), len(c.want), got)
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Fatalf("ascii=%v span %d = %+v, want %+v", c.ascii, i, got[i], c.want[i])
			}
		}
		if got.Text() != c.text {
			t.Fatalf("ascii=%v: Text() = %q, want %q", c.ascii, got.Text(), c.text)
		}
		if w := StatusSegmentWidth(c.ascii); w != 10 {
			t.Fatalf("ascii=%v: StatusSegmentWidth = %d, want 10", c.ascii, w)
		}
		if w := StatusSegmentWidth(c.ascii); w != textwidth.Width(got.Text()) {
			t.Fatalf("ascii=%v: StatusSegmentWidth = %d, disagrees with rendered width %d",
				c.ascii, w, textwidth.Width(got.Text()))
		}
	}
}

// TestUnit_StatusSegmentGolden keeps the identity's encoding reviewable next to the header's.
func TestUnit_StatusSegmentGolden(t *testing.T) {
	testkit.Golden(t, "status_segment", testkit.EncodeLines([]frame.Line{
		StatusSegment(false),
		StatusSegment(true),
	}))
}

// TestUnit_CompactBoundary pins that the layout switches exactly at CompactWidth, not one column either side.
func TestUnit_CompactBoundary(t *testing.T) {
	if n := len(Welcome(CompactWidth-1, Info{})); n != 3 {
		t.Fatalf("width %d: %d lines, want 2 compact rows + separator", CompactWidth-1, n)
	}
	if n := len(Welcome(CompactWidth, Info{})); n != 8 {
		t.Fatalf("width %d: %d lines, want 7 full rows + separator", CompactWidth, n)
	}
	// The session line is a full-variant addition only.
	if n := len(Welcome(CompactWidth, sampleInfo(false))); n != 9 {
		t.Fatalf("width %d with model: %d lines, want 8 full rows + separator", CompactWidth, n)
	}
	if n := len(Welcome(CompactWidth-1, sampleInfo(false))); n != 3 {
		t.Fatalf("width %d with model: %d lines, want 2 compact rows + separator", CompactWidth-1, n)
	}
}

func lineTexts(lines []frame.Line) string {
	var b strings.Builder
	for _, l := range lines {
		b.WriteString(l.Text())
		b.WriteByte('\n')
	}
	return b.String()
}

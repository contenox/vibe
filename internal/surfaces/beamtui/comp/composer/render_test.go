package composer

import (
	"fmt"
	"strings"
	"testing"

	"github.com/contenox/beam/internal/surfaces/beamtui/frame"
	"github.com/contenox/beam/internal/surfaces/beamtui/testkit"
	"github.com/contenox/beam/internal/surfaces/beamtui/textwidth"
)

// goldenWidths is the blueprint's resize matrix: narrow, the default
// terminal, and wide.
var goldenWidths = []int{60, 80, 120}

// encode renders and packages the result as a frame so the golden pins the
// caret next to the rows it belongs to — the caret IS the contract for a
// scrolled composer.
func encode(c *Composer, width int, focused, ascii bool) string {
	lines := c.Render(width, focused, ascii)
	row, col := c.CursorPos()
	return testkit.EncodeFrame(frame.Frame{
		Live:   lines,
		Cursor: frame.Cursor{Row: row, Col: col, Hidden: !focused},
	})
}

// draft3 is a three-line draft with the caret mid-line on the wide-rune
// line, so the golden pins cell columns and not just rune counts.
func draft3() *Composer {
	c := New()
	c.SetDraft("summarize the failing test\nthen 日本語 explain the fix\nand stop")
	c.Home()
	c.CursorUp()
	c.Home()
	c.WordRight()
	c.WordRight()
	return c
}

// draft8 is taller than MaxRows with the caret on the last line: the
// composer must scroll and keep the caret visible.
func draft8() *Composer {
	c := New()
	var b strings.Builder
	for i := 1; i <= 8; i++ {
		if i > 1 {
			b.WriteByte('\n')
		}
		fmt.Fprintf(&b, "draft line %d", i)
	}
	c.SetDraft(b.String())
	return c
}

// TestUnit_RenderGoldens pins the composer's shape in every state the
// app-shell can put it in.
func TestUnit_RenderGoldens(t *testing.T) {
	states := []struct {
		name    string
		build   func() *Composer
		focused bool
		ascii   bool
	}{
		{name: "empty_focused", build: New, focused: true},
		{name: "empty_unfocused", build: New},
		{name: "empty_unfocused_ascii", build: New, ascii: true},
		{name: "draft3_focused", build: draft3, focused: true},
		{name: "draft3_focused_ascii", build: draft3, focused: true, ascii: true},
		{name: "draft3_unfocused", build: draft3},
		{name: "draft8_scrolled", build: draft8, focused: true},
		{name: "draft8_scrolled_ascii", build: draft8, focused: true, ascii: true},
	}

	for _, s := range states {
		for _, w := range goldenWidths {
			name := fmt.Sprintf("%s_w%d", s.name, w)
			t.Run(name, func(t *testing.T) {
				testkit.Golden(t, name, encode(s.build(), w, s.focused, s.ascii))
			})
		}
	}
}

// TestUnit_RenderNarrowGolden is the width-20 case: wrapping does the work,
// the sigil column survives, and the placeholder gets an ellipsis.
func TestUnit_RenderNarrowGolden(t *testing.T) {
	long := New()
	long.SetDraft("wrap this prompt over several narrow rows 日本語 included")

	cases := []struct {
		name    string
		c       *Composer
		focused bool
	}{
		{"narrow_empty_unfocused_w20", New(), false},
		{"narrow_wrapped_focused_w20", long, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			testkit.Golden(t, tc.name, encode(tc.c, 20, tc.focused, false))
		})
	}
}

// renderStates is the corpus the width and caret properties run over.
func renderStates() []struct {
	name string
	c    *Composer
} {
	mk := func(s string) *Composer {
		c := New()
		c.SetDraft(s)
		return c
	}
	return []struct {
		name string
		c    *Composer
	}{
		{"empty", New()},
		{"short", mk("hello")},
		{"three lines", mk("alpha\nbeta\ngamma")},
		{"long paragraph", mk(strings.Repeat("wrap me around the composer column ", 8))},
		{"unbroken word", mk(strings.Repeat("x", 400))},
		{"wide runes", mk(strings.Repeat("日本語テキスト", 30))},
		{"emoji", mk(strings.Repeat("🙂🚀 ", 40))},
		{"mixed multiline", mk("prompt 日本語 🙂\n" + strings.Repeat("second line is long ", 10) + "\nthird")},
		{"blank lines", mk("\n\n\n")},
		{"tall", draft8()},
	}
}

// TestUnit_RenderNeverExceedsWidth is the resize property the goldens can
// only sample: at every supported width, in either focus state and either
// character set, no rendered line may spill a cell and the composer may
// never exceed MaxRows.
//
// It runs from 6 rather than 20 — two cells of sigil plus four of content —
// because that is where the arithmetic is actually interesting: at 20 the
// content column is 18 cells and nothing is ever tight. MinWidth itself (4)
// is excluded from the SPILL half only because a two-cell rune in a two-cell
// column is exact and one cell narrower is not; the no-text-loss test below
// covers what happens beneath it.
func TestUnit_RenderNeverExceedsWidth(t *testing.T) {
	for _, s := range renderStates() {
		t.Run(s.name, func(t *testing.T) {
			for w := 6; w <= 140; w++ {
				for _, focused := range []bool{true, false} {
					for _, ascii := range []bool{true, false} {
						lines := s.c.Render(w, focused, ascii)
						if len(lines) < 1 || len(lines) > MaxRows {
							t.Fatalf("width %d focused=%v: %d lines, want 1..%d", w, focused, len(lines), MaxRows)
						}
						for i, l := range lines {
							if got := textwidth.Width(l.Text()); got > w {
								t.Fatalf("width %d focused=%v line %d: %d cells (%q)", w, focused, i, got, l.Text())
							}
						}
					}
				}
			}
		})
	}
}

// TestUnit_CursorAlwaysVisible: the caret must land inside the rows Render
// just returned, at or after the sigil column — that is what "scroll keeps
// the cursor row visible" means operationally.
func TestUnit_CursorAlwaysVisible(t *testing.T) {
	for _, s := range renderStates() {
		t.Run(s.name, func(t *testing.T) {
			for w := 6; w <= 140; w++ {
				// Walk the caret through the whole buffer, not just its end.
				for step := 0; step < 40; step++ {
					lines := s.c.Render(w, true, false)
					row, col := s.c.CursorPos()
					if row < 0 || row >= len(lines) {
						t.Fatalf("width %d step %d: caret row %d outside %d lines", w, step, row, len(lines))
					}
					if col < prefixWidth || col >= w {
						t.Fatalf("width %d step %d: caret col %d outside [%d, %d)", w, step, col, prefixWidth, w)
					}
					s.c.CursorLeft()
				}
				s.c.SetDraft(s.c.Draft()) // caret back to the end
			}
		})
	}
}

// TestUnit_RenderEmptyStates pins the two empty renderings: focused shows a
// clean caret behind the sigil, unfocused teaches the three triggers.
func TestUnit_RenderEmptyStates(t *testing.T) {
	c := New()

	lines := c.Render(80, true, false)
	if len(lines) != 1 || lines[0].Text() != sigilUnicode {
		t.Fatalf("focused empty = %q, want just the sigil", testkit.EncodeLines(lines))
	}
	if row, col := c.CursorPos(); row != 0 || col != prefixWidth {
		t.Fatalf("focused empty caret = (%d, %d), want (0, %d)", row, col, prefixWidth)
	}
	if lines[0][0].Style != frame.StyleBrand {
		t.Fatalf("focused sigil style = %q, want brand", lines[0][0].Style)
	}

	lines = c.Render(80, false, false)
	if len(lines) != 1 {
		t.Fatalf("unfocused empty = %d lines, want 1", len(lines))
	}
	if want := sigilUnicode + placeholderUnicode; lines[0].Text() != want {
		t.Fatalf("unfocused empty = %q, want %q", lines[0].Text(), want)
	}
	for _, s := range lines[0] {
		if s.Style != frame.StyleMuted {
			t.Fatalf("unfocused empty span %q style = %q, want muted", s.Text, s.Style)
		}
	}

	// Narrow enough to cut the hint: it must still fit and say there is more.
	lines = c.Render(24, false, false)
	if got := textwidth.Width(lines[0].Text()); got > 24 {
		t.Fatalf("placeholder at width 24 = %d cells", got)
	}
	if !strings.HasSuffix(lines[0].Text(), ellipsisUnicode) {
		t.Fatalf("truncated placeholder = %q, want an ellipsis", lines[0].Text())
	}
}

// TestUnit_RenderASCIIStaysASCII: a Mono terminal must never receive the
// beam-bar or the middot.
func TestUnit_RenderASCIIStaysASCII(t *testing.T) {
	for _, s := range renderStates() {
		for _, focused := range []bool{true, false} {
			for _, w := range []int{20, 60, 80, 120} {
				for _, l := range s.c.Render(w, focused, true) {
					for _, span := range l {
						if strings.ContainsAny(span.Text, "▌…·") {
							t.Fatalf("%s w%d focused=%v: ASCII render contains %q", s.name, w, focused, span.Text)
						}
					}
				}
			}
		}
	}
}

// TestUnit_RenderSigilColumn: the first row carries the beam-bar, every
// continuation carries the two-cell gutter, and focus is the only thing that
// changes the sigil's style.
func TestUnit_RenderSigilColumn(t *testing.T) {
	c := draft3()
	for _, tc := range []struct {
		focused bool
		style   frame.StyleID
	}{{true, frame.StyleBrand}, {false, frame.StyleMuted}} {
		lines := c.Render(60, tc.focused, false)
		if len(lines) != 3 {
			t.Fatalf("focused=%v: %d lines, want 3", tc.focused, len(lines))
		}
		if lines[0][0] != frame.S(tc.style, sigilUnicode) {
			t.Fatalf("focused=%v: first prefix = %+v, want %q under %q", tc.focused, lines[0][0], sigilUnicode, tc.style)
		}
		for i, l := range lines[1:] {
			if l[0] != frame.S(frame.StyleMuted, continuation) {
				t.Fatalf("focused=%v: continuation %d prefix = %+v, want a muted gutter", tc.focused, i+1, l[0])
			}
		}
	}
}

// TestUnit_RenderScrollMarkerCountsHiddenRows: a draft taller than MaxRows
// scrolls, and used to do it silently — the rows above the window vanished
// with nothing on screen saying they had ever been typed. The first
// continuation row's gutter now counts them, inside the two cells the sigil
// column already costs, so no content row is spent on the news.
func TestUnit_RenderScrollMarkerCountsHiddenRows(t *testing.T) {
	c := draft8() // eight rows, caret on the last: two are hidden above
	lines := c.Render(60, true, false)
	if got, want := lines[1][0], frame.S(frame.StyleMuted, "↑2"); got != want {
		t.Fatalf("scroll marker = %+v, want %+v", got, want)
	}
	if got := lines[2][0]; got != frame.S(frame.StyleMuted, continuation) {
		t.Fatalf("only the FIRST continuation carries the marker; row 2 = %+v", got)
	}
	if got := textwidth.Width(lines[1][0].Text); got != prefixWidth {
		t.Fatalf("marker is %d cells, want exactly the %d-cell prefix", got, prefixWidth)
	}

	ascii := draft8().Render(60, true, true)
	if got, want := ascii[1][0], frame.S(frame.StyleMuted, "^2"); got != want {
		t.Fatalf("ascii scroll marker = %+v, want %+v", got, want)
	}

	// Scrolled back to the top there is nothing above, and the gutter is a
	// plain gutter again.
	for i := 0; i < 7; i++ {
		c.CursorUp()
	}
	lines = c.Render(60, true, false)
	if got := lines[1][0]; got != frame.S(frame.StyleMuted, continuation) {
		t.Fatalf("unscrolled composer drew a scroll marker: %+v", got)
	}

	// Past nine hidden rows the count degrades rather than stealing a cell
	// from the content column.
	tall := New()
	tall.SetDraft(strings.Repeat("a line of draft\n", 20) + "end")
	lines = tall.Render(60, true, false)
	if got, want := lines[1][0], frame.S(frame.StyleMuted, "↑+"); got != want {
		t.Fatalf("deep scroll marker = %+v, want %+v", got, want)
	}
}

// TestUnit_RenderScrollsToTheCaret: a draft taller than MaxRows shows the
// window the caret is in, and moving the caret up moves the window.
func TestUnit_RenderScrollsToTheCaret(t *testing.T) {
	c := draft8()

	lines := c.Render(60, true, false)
	if len(lines) != MaxRows {
		t.Fatalf("%d lines, want %d", len(lines), MaxRows)
	}
	if got, want := lines[MaxRows-1].Text(), continuation+"draft line 8"; got != want {
		t.Fatalf("last row = %q, want %q", got, want)
	}
	if row, _ := c.CursorPos(); row != MaxRows-1 {
		t.Fatalf("caret row = %d, want the last row %d", row, MaxRows-1)
	}

	// Walking the caret to the top scrolls the window back to line 1.
	for i := 0; i < 7; i++ {
		c.CursorUp()
	}
	lines = c.Render(60, true, false)
	if got, want := lines[0].Text(), sigilUnicode+"draft line 1"; got != want {
		t.Fatalf("first row = %q, want %q", got, want)
	}
	if row, _ := c.CursorPos(); row != 0 {
		t.Fatalf("caret row = %d, want the first row", row)
	}
}

// TestUnit_RenderUsesOnlyClosedStyleIDs enforces frame's closed set.
func TestUnit_RenderUsesOnlyClosedStyleIDs(t *testing.T) {
	known := map[frame.StyleID]bool{}
	for _, id := range frame.All() {
		known[id] = true
	}
	for _, s := range renderStates() {
		for _, focused := range []bool{true, false} {
			for _, l := range s.c.Render(80, focused, false) {
				for _, span := range l {
					if !known[span.Style] {
						t.Fatalf("%s: span %q uses unknown StyleID %q", s.name, span.Text, span.Style)
					}
				}
			}
		}
	}
}

// TestUnit_RenderWrapPreservesText: soft wrap is presentation only — the
// rendered rows concatenate back to the buffer line they came from, so
// nothing typed is ever lost on screen.
//
// The floor is 20 on purpose and is not the supported minimum: narrower than
// that, this particular text needs more than MaxRows rows and the composer
// SCROLLS, which drops rows from the render by design. Text loss below
// MaxRows is what TestUnit_NarrowWidthNeverDeletesText covers, with drafts
// short enough that scrolling cannot be the explanation.
func TestUnit_RenderWrapPreservesText(t *testing.T) {
	const text = "the quick brown fox jumps over the lazy dog 日本語テキストの行"
	c := New()
	c.SetDraft(text)

	for w := 20; w <= 140; w++ {
		var b strings.Builder
		for _, l := range c.Render(w, true, false) {
			if len(l) > 1 {
				b.WriteString(l[1].Text)
			}
		}
		if got := b.String(); got != text {
			t.Fatalf("width %d: rows join to %q, want %q", w, got, text)
		}
	}
}

// TestUnit_NarrowWidthNeverDeletesText is the rule below MinWidth. At three
// columns the content column is one cell, and a two-cell rune cannot be
// broken across two one-cell rows — the wrapper emits it and the row
// overflows, which is the only thing it can do.
//
// The composer must not "fix" that by cutting. A truncation there would
// delete a character out of the user's own draft while CursorPos went on
// counting it, so the caret would sit a cell off and backspace would look
// broken. Overflowing a three-column terminal is cosmetic; eating what
// somebody typed is not.
//
// Drafts here are short enough to fit inside MaxRows at one cell per row, so
// a missing rune is a deletion and not the documented scroll.
func TestUnit_NarrowWidthNeverDeletesText(t *testing.T) {
	drafts := []string{"abc", "日本語", "🙂x", "a日", "  "}
	for _, draft := range drafts {
		for w := 3; w <= 12; w++ {
			for _, ascii := range []bool{false, true} {
				c := New()
				c.SetDraft(draft)
				var b strings.Builder
				for _, l := range c.Render(w, true, ascii) {
					if len(l) > 1 {
						b.WriteString(l[1].Text)
					}
				}
				if got := b.String(); got != draft {
					t.Fatalf("draft %q at width %d (ascii=%v): rows join to %q — text was deleted",
						draft, w, ascii, got)
				}
			}
		}
	}
}

// TestUnit_MinWidthIsExact: at and above MinWidth the width invariant holds
// with no exceptions, which is what makes MinWidth a supported width rather
// than a number in a comment.
func TestUnit_MinWidthIsExact(t *testing.T) {
	for _, s := range renderStates() {
		for _, focused := range []bool{true, false} {
			for _, ascii := range []bool{true, false} {
				for _, l := range s.c.Render(MinWidth, focused, ascii) {
					if got := textwidth.Width(l.Text()); got > MinWidth {
						t.Fatalf("%s focused=%v ascii=%v: %d cells at MinWidth (%q)",
							s.name, focused, ascii, got, l.Text())
					}
				}
			}
		}
	}
}

// TestUnit_PasteIsSanitized: the clipboard is an ingest point like any other,
// and its contents are whatever was last copied — including out of a terminal
// showing somebody else's output.
func TestUnit_PasteIsSanitized(t *testing.T) {
	c := New()
	c.InsertString("ev\x1b[2Jil\x1b]0;t\x07\ttab\x7f‮flip\nsecond\x1bline")

	for _, r := range c.Draft() {
		if r == '\n' {
			continue // a paste's newlines are line breaks, and are kept
		}
		if r < 0x20 || r == 0x7f {
			t.Fatalf("draft %q carries %U", c.Draft(), r)
		}
		if (r >= 0x202a && r <= 0x202e) || (r >= 0x2066 && r <= 0x2069) {
			t.Fatalf("draft %q carries bidi control %U", c.Draft(), r)
		}
	}
	for _, l := range c.Render(80, true, false) {
		for _, s := range l {
			for _, r := range s.Text {
				if r < 0x20 || r == 0x7f {
					t.Fatalf("span %q carries %U", s.Text, r)
				}
			}
		}
	}

	// A single keystroke takes the other path and must agree with it.
	d := New()
	for _, r := range []rune{'a', 0x1b, 0x202e, 0x7f, 'b'} {
		d.InsertRune(r)
	}
	if got := d.Draft(); got != "ab" {
		t.Fatalf("InsertRune draft = %q, want %q", got, "ab")
	}
}

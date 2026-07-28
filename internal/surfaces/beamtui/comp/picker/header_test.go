package picker

import (
	"fmt"
	"strings"
	"testing"

	"github.com/contenox/beam/internal/surfaces/beamtui/testkit"
	"github.com/contenox/beam/internal/surfaces/beamtui/textwidth"
)

// TestUnit_PickerHeader_Goldens: the header is the overlay's first row,
// muted, inside the row budget.
func TestUnit_PickerHeader_Goldens(t *testing.T) {
	scenarios := []struct {
		name    string
		header  string
		maxRows int
		query   string
	}{
		{"header-short", "/src", 5, ""},
		{"header-long", "/internal/surfaces/beamtui/comp/fileaddr/deeply/nested/further", 5, ""},
		{"header-empty-list", "/src/nested", 5, "zzzzzz-no-such-file"},
	}
	for _, sc := range scenarios {
		for _, w := range goldenWidths {
			for _, ascii := range []bool{false, true} {
				name := fmt.Sprintf("%s-w%d", sc.name, w)
				if ascii {
					name += "-ascii"
				}
				t.Run(name, func(t *testing.T) {
					p := New()
					p.SetItems(sampleItems())
					p.SetQuery(sc.query)
					p.SetHeader(sc.header)
					lines := p.Render(w, sc.maxRows, ascii)
					if len(lines) > sc.maxRows {
						t.Fatalf("rendered %d lines, maxRows is %d", len(lines), sc.maxRows)
					}
					for i, l := range lines {
						if got := textwidth.Width(l.Text()); got > w {
							t.Fatalf("line %d is %d cells wide, width is %d: %q", i, got, w, l.Text())
						}
					}
					testkit.Golden(t, name, testkit.EncodeLines(lines))
				})
			}
		}
	}
}

// TestUnit_PickerHeader_CostsARowFromTheBudget: the header is inside maxRows,
// not on top of it, same as the "+N more" footer.
func TestUnit_PickerHeader_CostsARowFromTheBudget(t *testing.T) {
	bare := New()
	bare.SetItems(sampleItems())
	withHeader := New()
	withHeader.SetItems(sampleItems())
	withHeader.SetHeader("/src")

	for _, rows := range []int{2, 3, 4, 8, 20} {
		got := withHeader.Render(80, rows, false)
		if len(got) > rows {
			t.Fatalf("maxRows=%d with a header rendered %d lines", rows, len(got))
		}
		if got[0].Text() != "/src" {
			t.Fatalf("maxRows=%d: first line = %q, want the header", rows, got[0].Text())
		}
		want := bare.Render(80, rows-1, false)
		if len(got)-1 != len(want) {
			t.Fatalf("maxRows=%d: %d rows under the header, want %d", rows, len(got)-1, len(want))
		}
		for i := range want {
			if got[i+1].Text() != want[i].Text() {
				t.Fatalf("maxRows=%d row %d = %q, want %q", rows, i, got[i+1].Text(), want[i].Text())
			}
		}
	}
}

// TestUnit_PickerHeader_DroppedWhenItWouldBeTheOnlyRow: a single available
// line goes to the content, not to a header naming a list the caller can't see.
func TestUnit_PickerHeader_DroppedWhenItWouldBeTheOnlyRow(t *testing.T) {
	p := New()
	p.SetItems(sampleItems())
	p.SetHeader("/src")
	one := p.Render(80, 1, false)
	if len(one) != 1 {
		t.Fatalf("maxRows=1 rendered %d lines, want 1", len(one))
	}
	if one[0].Text() == "/src" {
		t.Fatal("maxRows=1 spent its only line on the header")
	}
	if !strings.Contains(one[0].Text(), "picker.go") {
		t.Fatalf("maxRows=1 line = %q, want the selected item", one[0].Text())
	}

	p.SetQuery("zzzzzz")
	if got := p.Render(80, 1, false); len(got) != 1 || got[0].Text() != defaultEmptyText {
		t.Fatalf("maxRows=1 empty state = %v, want the empty text alone", got)
	}
}

// TestUnit_PickerHeader_EmptyIsTheOldBehaviourExactly: an unset or cleared
// header leaves every render byte-identical to before headers existed.
func TestUnit_PickerHeader_EmptyIsTheOldBehaviourExactly(t *testing.T) {
	for _, rows := range []int{1, 2, 4, 9} {
		for _, w := range []int{20, 60, 80} {
			bare := New()
			bare.SetItems(sampleItems())
			cleared := New()
			cleared.SetItems(sampleItems())
			cleared.SetHeader("/src/nested")
			cleared.SetHeader("")
			if cleared.Header() != "" {
				t.Fatalf("SetHeader(\"\") left %q", cleared.Header())
			}
			a := testkit.EncodeLines(bare.Render(w, rows, false))
			b := testkit.EncodeLines(cleared.Render(w, rows, false))
			if a != b {
				t.Fatalf("rows=%d w=%d: a cleared header changed the render:\n%s\n---\n%s", rows, w, a, b)
			}
		}
	}
}

// TestUnit_PickerHeader_NeverExceedsWidthOrRows walks degenerate sizes with an
// oversized, wide-rune header, in both glyph modes.
func TestUnit_PickerHeader_NeverExceedsWidthOrRows(t *testing.T) {
	headers := []string{
		"/src",
		"/" + strings.Repeat("very-long-segment/", 12),
		"/日本語/ディレクトリ/名前",
		"/🙂/🚀",
	}
	for _, h := range headers {
		for _, ascii := range []bool{false, true} {
			for _, rows := range []int{1, 2, 3, 8} {
				for _, sel := range []int{0, 3, 99} {
					p := New()
					p.SetItems(sampleItems())
					p.SetHeader(h)
					p.Move(sel)
					for w := 1; w <= 120; w++ {
						lines := p.Render(w, rows, ascii)
						if len(lines) > rows {
							t.Fatalf("header %q ascii=%v rows=%d w=%d: %d lines", h, ascii, rows, w, len(lines))
						}
						for i, l := range lines {
							if got := textwidth.Width(l.Text()); got > w {
								t.Fatalf("header %q ascii=%v rows=%d w=%d line %d: %d cells (%q)",
									h, ascii, rows, w, i, got, l.Text())
							}
						}
					}
				}
			}
		}
	}
}

// TestUnit_PickerHeader_IsSanitized: a header's source may be a directory
// name carrying nearly anything.
func TestUnit_PickerHeader_IsSanitized(t *testing.T) {
	p := New()
	p.SetItems(sampleItems())
	p.SetHeader("/ev\x1b[2Jil\x07\tdir\x7f‮")
	for _, l := range p.Render(200, 6, false) {
		for _, s := range l {
			for _, r := range s.Text {
				if r < 0x20 || r == 0x7f {
					t.Fatalf("header span %q carries %U", s.Text, r)
				}
				if (r >= 0x202a && r <= 0x202e) || (r >= 0x2066 && r <= 0x2069) {
					t.Fatalf("header span %q carries bidi control %U", s.Text, r)
				}
			}
		}
	}
}

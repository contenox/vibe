package picker

import (
	"fmt"
	"strings"
	"testing"

	"github.com/contenox/beam/internal/surfaces/beamtui/testkit"
	"github.com/contenox/beam/internal/surfaces/beamtui/textwidth"
)

// goldenWidths is the blueprint's resize matrix for an overlay that lives in
// the live region: the narrow terminal and the default one.
var goldenWidths = []int{60, 80}

func TestUnit_PickerRank_TierOrder(t *testing.T) {
	cases := []struct {
		name  string
		query string
		label string
		want  int
		match bool
	}{
		{"basename prefix", "pick", "internal/comp/picker.go", RankBasenamePrefix, true},
		{"basename prefix beats the same query being a path substring",
			"comp", "internal/comp/comp.go", RankBasenamePrefix, true},
		{"path substring when the basename does not start with it",
			"comp", "internal/comp/picker.go", RankPathSubstring, true},
		{"path substring at the very start of the path",
			"internal", "internal/comp/picker.go", RankPathSubstring, true},
		{"subsequence with gaps", "icp", "internal/comp/picker.go", RankSubsequence, true},
		{"subsequence spanning separators", "ipg", "internal/picker.go", RankSubsequence, true},
		{"no match when a rune is missing", "zzz", "internal/comp/picker.go", 0, false},
		{"no match when the order is wrong", "gp", "picker.go", 0, false},
		{"empty query matches everything at RankAny", "", "anything", RankAny, true},
		{"label with no slash is its own basename", "ses", "session-alpha", RankBasenamePrefix, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := Rank(tc.query, tc.label)
			if ok != tc.match {
				t.Fatalf("Rank(%q, %q) matched=%v, want %v", tc.query, tc.label, ok, tc.match)
			}
			if ok && got != tc.want {
				t.Fatalf("Rank(%q, %q) = tier %d, want %d", tc.query, tc.label, got, tc.want)
			}
		})
	}
}

func TestUnit_PickerRank_CaseInsensitive(t *testing.T) {
	pairs := [][2]string{
		{"PICK", "internal/comp/picker.go"},
		{"pick", "internal/comp/PICKER.go"},
		{"PiCkEr", "internal/comp/pIcKeR.go"},
	}
	for _, p := range pairs {
		got, ok := Rank(p[0], p[1])
		if !ok || got != RankBasenamePrefix {
			t.Fatalf("Rank(%q, %q) = (%d, %v), want (%d, true)",
				p[0], p[1], got, ok, RankBasenamePrefix)
		}
	}
	// Case-insensitivity must hold for the weaker tiers too.
	if got, ok := Rank("COMP", "internal/comp/picker.go"); !ok || got != RankPathSubstring {
		t.Fatalf("Rank(COMP, ...) = (%d, %v), want (%d, true)", got, ok, RankPathSubstring)
	}
	if got, ok := Rank("ICP", "internal/comp/picker.go"); !ok || got != RankSubsequence {
		t.Fatalf("Rank(ICP, ...) = (%d, %v), want (%d, true)", got, ok, RankSubsequence)
	}
}

func TestUnit_PickerFilter_TiersThenLabel(t *testing.T) {
	items := []Item{
		{ID: "1", Label: "zzz/other.go"},         // subsequence: o..e via "other"? no — see below
		{ID: "2", Label: "internal/comp/one.go"}, // path substring "comp"
		{ID: "3", Label: "comp/beta.go"},         // path substring "comp"
		{ID: "4", Label: "comp.go"},              // basename prefix
		{ID: "5", Label: "acompb.go"},            // basename prefix? no: substring
		{ID: "6", Label: "c/o/m/p.go"},           // subsequence only
		{ID: "7", Label: "nothing"},              // no match
	}
	got := Filter(items, "comp")
	var labels []string
	for _, it := range got {
		labels = append(labels, it.Label)
	}
	want := []string{
		// tier 0 (basename prefix)
		"comp.go",
		// tier 1 (path substring), sorted by label
		"acompb.go", "comp/beta.go", "internal/comp/one.go",
		// tier 2 (subsequence)
		"c/o/m/p.go",
	}
	if strings.Join(labels, ",") != strings.Join(want, ",") {
		t.Fatalf("Filter order:\n got %v\nwant %v", labels, want)
	}
	// Rank is an output field: every survivor carries the tier it matched at.
	if got[0].Rank != RankBasenamePrefix {
		t.Fatalf("first item Rank = %d, want %d", got[0].Rank, RankBasenamePrefix)
	}
	if got[len(got)-1].Rank != RankSubsequence {
		t.Fatalf("last item Rank = %d, want %d", got[len(got)-1].Rank, RankSubsequence)
	}
}

func TestUnit_PickerFilter_StableWithinTier(t *testing.T) {
	// Duplicate labels in one tier must keep input order, so a caller's
	// tie-break (recency, first-seen) is never scrambled.
	items := []Item{
		{ID: "first", Label: "dup.go"},
		{ID: "second", Label: "dup.go"},
		{ID: "third", Label: "dup.go"},
	}
	got := Filter(items, "dup")
	if len(got) != 3 {
		t.Fatalf("got %d items, want 3", len(got))
	}
	for i, want := range []string{"first", "second", "third"} {
		if got[i].ID != want {
			t.Fatalf("position %d = %q, want %q (tie order must be stable)", i, got[i].ID, want)
		}
	}
}

func TestUnit_PickerFilter_EmptyQueryPreservesCallerOrder(t *testing.T) {
	// The session picker depends on this: an unqueried roster must stay in
	// the order engine-bridge handed it over, not become alphabetical.
	items := []Item{
		{ID: "c", Label: "zeta"},
		{ID: "a", Label: "alpha"},
		{ID: "b", Label: "mid"},
	}
	got := Filter(items, "")
	if len(got) != 3 {
		t.Fatalf("got %d items, want 3", len(got))
	}
	for i, want := range []string{"zeta", "alpha", "mid"} {
		if got[i].Label != want {
			t.Fatalf("position %d = %q, want %q", i, got[i].Label, want)
		}
		if got[i].Rank != RankAny {
			t.Fatalf("position %d Rank = %d, want RankAny", i, got[i].Rank)
		}
	}
}

// sampleItems is the shared fixture for the render goldens: enough rows to
// force a window and a footer, with details of varying length.
func sampleItems() []Item {
	paths := []string{
		"picker.go",
		"internal/surfaces/beamtui/comp/picker/picker.go",
		"internal/surfaces/beamtui/comp/fileaddr/fileaddr.go",
		"internal/surfaces/beamtui/frame/frame.go",
		"internal/services/vfs/vfs.go",
		"docs/development/blueprints/beam-tui.md",
		"cmd/beam/main.go",
		"README.md",
	}
	items := make([]Item, 0, len(paths))
	for _, p := range paths {
		dir := ""
		if i := strings.LastIndex(p, "/"); i >= 0 {
			dir = p[:i]
		}
		items = append(items, Item{ID: p, Label: p, Detail: dir})
	}
	return items
}

func TestUnit_PickerRender_Goldens(t *testing.T) {
	scenarios := []struct {
		name    string
		maxRows int
		move    int
		query   string
	}{
		// Everything fits: no footer, selection on the first row.
		{"fits", 10, 0, ""},
		// More items than rows: window at the top plus the "+N more" footer.
		{"capped-top", 4, 0, ""},
		// Selection pushed into the middle: the window scrolls with it.
		{"capped-middle", 4, 3, ""},
		// Selection at the very end: the window clamps to the list bottom.
		{"capped-end", 4, 99, ""},
		// A query reorders into tiers before any of the above applies.
		{"queried", 5, 0, "picker"},
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
					p.Move(sc.move)
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

func TestUnit_PickerRender_EmptyState(t *testing.T) {
	p := New()
	p.SetItems(sampleItems())
	p.SetQuery("zzzzzz-no-such-file")
	if p.FilteredLen() != 0 {
		t.Fatalf("expected no matches, got %d", p.FilteredLen())
	}
	lines := p.Render(80, 6, false)
	if len(lines) != 1 {
		t.Fatalf("empty state rendered %d lines, want exactly 1", len(lines))
	}
	if got := lines[0].Text(); got != defaultEmptyText {
		t.Fatalf("default empty text = %q, want %q", got, defaultEmptyText)
	}
	testkit.Golden(t, "empty-default", testkit.EncodeLines(lines))

	// The caller owns the text — file-addressing's fixed no-root state comes
	// through here.
	p.SetEmptyText("no workspace root — @ needs a granted directory")
	lines = p.Render(80, 6, false)
	testkit.Golden(t, "empty-custom", testkit.EncodeLines(lines))

	// An empty item set is the same state, not a panic.
	q := New()
	if lines := q.Render(80, 6, false); len(lines) != 1 {
		t.Fatalf("empty item set rendered %d lines, want 1", len(lines))
	}
	if _, ok := q.Selected(); ok {
		t.Fatal("Selected reported ok on an empty picker")
	}
	if q.SelectedIndex() != -1 {
		t.Fatalf("SelectedIndex = %d on an empty picker, want -1", q.SelectedIndex())
	}
}

// TestUnit_PickerRender_FooterCountsHiddenItems: the footer counts each
// direction separately, because each number answers a different question —
// what scrolling down would reach, and what scrolling up would.
func TestUnit_PickerRender_FooterCountsHiddenItems(t *testing.T) {
	p := New()
	p.SetItems(sampleItems())
	lines := p.Render(80, 4, false)
	if len(lines) != 4 {
		t.Fatalf("got %d lines, want 4", len(lines))
	}
	// Window at the top: 3 item rows + footer, out of 8 items => 5 below and
	// nothing above, so the note says one thing. It is indented to the row
	// column, so it reads as a note about the list, not as another row of it.
	if got, want := lines[3].Text(), "  +5 more"; got != want {
		t.Fatalf("footer = %q, want %q", got, want)
	}

	// Scrolled into the middle, the below-count drops and an above-count
	// appears: items ABOVE the window are not below it, and saying otherwise
	// sends the operator hunting for rows that are behind them.
	p.Move(3)
	lines = p.Render(80, 4, false)
	if len(lines) != 4 {
		t.Fatalf("scrolled: got %d lines, want 4", len(lines))
	}
	if got, want := lines[3].Text(), "  ↑2 above  +3 more"; got != want {
		t.Fatalf("scrolled footer = %q, want %q", got, want)
	}

	// At the bottom nothing is below the window, which is exactly the moment
	// the rows above are the ONLY hidden ones. The footer stands and says so
	// rather than handing its line back and leaving them unannounced.
	p.Move(99)
	lines = p.Render(80, 4, false)
	if len(lines) != 4 {
		t.Fatalf("at the end: got %d lines, want 4", len(lines))
	}
	if got, want := lines[3].Text(), "  ↑5 above"; got != want {
		t.Fatalf("footer at the end = %q, want %q", got, want)
	}
	if strings.Contains(lines[3].Text(), "more") {
		t.Fatalf("footer at the end still counts rows below: %q", lines[3].Text())
	}
	if last := lines[2].Text(); !strings.Contains(last, "README.md") {
		t.Fatalf("last item row = %q, want the final item", last)
	}

	// Mono gets the same note with the caret the rest of the live region
	// falls back to.
	if got, want := p.Render(80, 4, true)[3].Text(), "  ^5 above"; got != want {
		t.Fatalf("ascii footer = %q, want %q", got, want)
	}

	// A single-row budget spends its one line on the item, not on a footer
	// that would have nothing above it.
	p.Move(-99)
	one := p.Render(80, 1, false)
	if len(one) != 1 {
		t.Fatalf("maxRows=1 rendered %d lines, want 1", len(one))
	}
	if strings.Contains(one[0].Text(), "more") || strings.Contains(one[0].Text(), "above") {
		t.Fatalf("maxRows=1 spent its only line on a footer: %q", one[0].Text())
	}
}

func TestUnit_PickerRender_DegenerateSizes(t *testing.T) {
	p := New()
	p.SetItems(sampleItems())
	for _, tc := range []struct{ w, rows int }{{0, 5}, {-1, 5}, {80, 0}, {80, -3}} {
		if lines := p.Render(tc.w, tc.rows, false); lines != nil {
			t.Fatalf("Render(%d, %d) = %v, want nil", tc.w, tc.rows, lines)
		}
	}
}

// TestUnit_PickerRender_NeverExceedsWidth walks every width from one cell up,
// in BOTH glyph modes, over the states that actually stress the arithmetic:
// a long label, a long detail, an empty state, a footer.
//
// ASCII from w=1 is the case that mattered. "..." is three cells where "…" is
// one, and a truncation helper handed a tail wider than its budget returns
// the bare tail — so the narrow ASCII rows overflowed while the unicode ones
// the old test sampled looked fine.
func TestUnit_PickerRender_NeverExceedsWidth(t *testing.T) {
	sets := map[string][]Item{
		"sample": sampleItems(),
		"long": {
			{ID: "a", Label: strings.Repeat("a-very-long-path-segment/", 10), Detail: strings.Repeat("detail ", 10)},
			{ID: "b", Label: "短", Detail: "日本語のディテール"},
			{ID: "c", Label: "🙂🚀 emoji label", Detail: "🙂"},
			{ID: "d", Label: "x", Detail: ""},
		},
		"empty": nil,
	}
	for name, items := range sets {
		for _, ascii := range []bool{false, true} {
			for _, rows := range []int{1, 2, 3, 8} {
				for _, sel := range []int{0, 2, 99} {
					p := New()
					p.SetItems(items)
					p.SetEmptyText("no workspace root — @ needs a granted directory")
					p.Move(sel)
					for w := 1; w <= 140; w++ {
						for i, l := range p.Render(w, rows, ascii) {
							if got := textwidth.Width(l.Text()); got > w {
								t.Fatalf("%s ascii=%v rows=%d sel=%d width %d line %d: %d cells (%q)",
									name, ascii, rows, sel, w, i, got, l.Text())
							}
						}
					}
				}
			}
		}
	}
}

// TestUnit_PickerRender_NeverExceedsMaxRows: the footer lives inside the row
// budget, so a caller that reserved maxRows lines gets at most that many at
// every width and every selection.
func TestUnit_PickerRender_NeverExceedsMaxRows(t *testing.T) {
	p := New()
	p.SetItems(sampleItems())
	for _, rows := range []int{1, 2, 3, 4, 7, 8, 9, 20} {
		for sel := 0; sel < 10; sel++ {
			p.Move(-99)
			p.Move(sel)
			for _, w := range []int{1, 5, 20, 80} {
				if got := len(p.Render(w, rows, false)); got > rows {
					t.Fatalf("maxRows=%d sel=%d width=%d: rendered %d lines", rows, sel, w, got)
				}
			}
		}
	}
}

// TestUnit_PickerItemsAreSanitized: a candidate's Label is a filename, and a
// filesystem will accept very nearly anything in one. ID is exempt on
// purpose — it is the caller's identity token and is never drawn.
func TestUnit_PickerItemsAreSanitized(t *testing.T) {
	const evil = "ev\x1b[2Jil\x1b]0;t\x07\tname\x7f‮txt"
	p := New()
	p.SetItems([]Item{{ID: evil, Label: evil, Detail: evil}})

	for _, ascii := range []bool{false, true} {
		for _, l := range p.Render(200, 4, ascii) {
			for _, s := range l {
				for _, r := range s.Text {
					if r < 0x20 || r == 0x7f {
						t.Fatalf("span %q carries %U", s.Text, r)
					}
					if (r >= 0x202a && r <= 0x202e) || (r >= 0x2066 && r <= 0x2069) {
						t.Fatalf("span %q carries bidi control %U", s.Text, r)
					}
				}
			}
		}
	}
	if sel, ok := p.Selected(); !ok || sel.ID != evil {
		t.Fatalf("Selected().ID = %q, want the caller's identity token untouched", sel.ID)
	}
}

func TestUnit_PickerMove_ClampsAndPages(t *testing.T) {
	p := New()
	p.SetItems(sampleItems())
	n := p.FilteredLen()
	if n != 8 {
		t.Fatalf("fixture has %d items, want 8", n)
	}
	if p.SelectedIndex() != 0 {
		t.Fatalf("initial selection = %d, want 0", p.SelectedIndex())
	}
	p.Move(-5)
	if p.SelectedIndex() != 0 {
		t.Fatalf("moving up past the top = %d, want 0 (clamped, never wrapped)", p.SelectedIndex())
	}
	p.Move(3)
	if p.SelectedIndex() != 3 {
		t.Fatalf("Move(3) = %d, want 3", p.SelectedIndex())
	}
	p.Move(100)
	if p.SelectedIndex() != n-1 {
		t.Fatalf("moving down past the end = %d, want %d (clamped)", p.SelectedIndex(), n-1)
	}

	// PageMove steps by the configured page size.
	p.SetPageSize(3)
	p.Move(-100)
	p.PageMove(1)
	if p.SelectedIndex() != 3 {
		t.Fatalf("PageMove(1) with pageSize 3 = %d, want 3", p.SelectedIndex())
	}
	p.PageMove(-1)
	if p.SelectedIndex() != 0 {
		t.Fatalf("PageMove(-1) = %d, want 0", p.SelectedIndex())
	}
	p.SetPageSize(0) // ignored
	p.PageMove(1)
	if p.SelectedIndex() != 3 {
		t.Fatalf("SetPageSize(0) changed the page size: index %d", p.SelectedIndex())
	}
}

func TestUnit_PickerSelected_TracksFilteredList(t *testing.T) {
	p := New()
	p.SetItems(sampleItems())
	p.Move(2)
	if p.Len() != 8 || p.FilteredLen() != 8 {
		t.Fatalf("Len/FilteredLen = %d/%d, want 8/8", p.Len(), p.FilteredLen())
	}

	// A query re-ranks AND resets the selection to the best match.
	p.SetQuery("picker")
	if p.SelectedIndex() != 0 {
		t.Fatalf("SetQuery left the selection at %d, want 0", p.SelectedIndex())
	}
	sel, ok := p.Selected()
	if !ok {
		t.Fatal("Selected reported not-ok with matches present")
	}
	// Both "picker.go" and ".../picker/picker.go" have the basename
	// "picker.go", so both are tier 0 and Label decides between them — the
	// documented within-tier rule, not path depth.
	if sel.Rank != RankBasenamePrefix {
		t.Fatalf("best match %q is tier %d, want %d", sel.Label, sel.Rank, RankBasenamePrefix)
	}
	if want := "internal/surfaces/beamtui/comp/picker/picker.go"; sel.Label != want {
		t.Fatalf("best match = %q, want %q (tier tie broken by Label)", sel.Label, want)
	}
	if p.Len() != 8 {
		t.Fatalf("Len = %d, want the unfiltered 8", p.Len())
	}
	if p.FilteredLen() >= 8 {
		t.Fatalf("FilteredLen = %d, want fewer than the 8 total", p.FilteredLen())
	}

	// Refreshing the item set under a live query re-applies it.
	p.SetItems([]Item{{ID: "x", Label: "picker.go"}, {ID: "y", Label: "unrelated"}})
	if p.FilteredLen() != 1 {
		t.Fatalf("after SetItems, FilteredLen = %d, want 1", p.FilteredLen())
	}
	if p.Query() != "picker" {
		t.Fatalf("SetItems lost the query: %q", p.Query())
	}
}

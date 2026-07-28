// Package picker is beam's generic keyboard list overlay: the filtered,
// ranked, windowed list every "choose one of these" surface renders through.
// It is pure — Render is a function of (state, width, maxRows) -> []frame.Line,
// with no I/O and no knowledge of what an item's ID represents. Ranking
// (Rank/Filter) lives here too, in one fixed tier order — basename prefix,
// path substring, fuzzy subsequence — with [FuzzyScore] breaking ties within
// the fuzzy tier.
package picker

import (
	"path"
	"sort"
	"strings"

	"github.com/contenox/contenox/internal/surfaces/beamtui/frame"
	"github.com/contenox/contenox/internal/surfaces/beamtui/sanitize"
	"github.com/contenox/contenox/internal/surfaces/beamtui/textwidth"
)

// Item is one selectable row. ID is the caller's stable identity and is
// never rendered; Label is the primary text ranking matches against; Detail
// is dimmed right-hand context. Rank is an output: Filter/SetQuery overwrite
// it with the match tier produced.
type Item struct {
	ID     string
	Label  string
	Detail string
	Rank   int
}

// Match tiers, best first: an exact basename prefix beats a substring
// anywhere in the path, which beats a fuzzy subsequence.
const (
	RankBasenamePrefix = 0 // query is a prefix of the label's last segment
	RankPathSubstring  = 1 // query appears contiguously anywhere in the label
	RankSubsequence    = 2 // query's runes appear in order, with gaps
	RankAny            = 3 // empty query: caller order is authoritative
)

// Rank returns the match tier of label under query, and whether it matched.
// Matching is case-insensitive; a label with no slash is its own basename.
func Rank(query, label string) (int, bool) {
	if query == "" {
		return RankAny, true
	}
	q := strings.ToLower(query)
	l := strings.ToLower(label)
	if strings.HasPrefix(path.Base(l), q) {
		return RankBasenamePrefix, true
	}
	if strings.Contains(l, q) {
		return RankPathSubstring, true
	}
	if subsequence(l, q) {
		return RankSubsequence, true
	}
	return 0, false
}

// subsequence reports whether every rune of needle appears in hay in order.
// Both are expected pre-lowercased by the caller.
func subsequence(hay, needle string) bool {
	if needle == "" {
		return true
	}
	h := []rune(hay)
	i := 0
	for _, r := range needle {
		found := false
		for i < len(h) {
			c := h[i]
			i++
			if c == r {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

// Filter ranks items against query, drops non-matches, and returns a new
// slice sorted by tier then Label, except the fuzzy subsequence tier, which
// is ordered by [FuzzyScore] best-first (falling back to Label on a tie). An
// empty query is the exception: nothing is dropped or reordered, so the
// caller's own order survives.
func Filter(items []Item, query string) []Item {
	out := make([]Item, 0, len(items))
	for _, it := range items {
		r, ok := Rank(query, it.Label)
		if !ok {
			continue
		}
		it.Rank = r
		out = append(out, it)
	}
	if query == "" {
		return out
	}
	// Scores computed once per item, not inside the O(n log n) comparator.
	var score []int
	for _, it := range out {
		if it.Rank != RankSubsequence {
			score = append(score, 0)
			continue
		}
		s, _ := FuzzyScore(query, it.Label)
		score = append(score, s)
	}
	idx := make([]int, len(out))
	for i := range idx {
		idx[i] = i
	}
	// Stable so items tying on (tier, score, Label) keep input order.
	sort.SliceStable(idx, func(a, b int) bool {
		i, j := idx[a], idx[b]
		if out[i].Rank != out[j].Rank {
			return out[i].Rank < out[j].Rank
		}
		if out[i].Rank == RankSubsequence && score[i] != score[j] {
			return score[i] > score[j]
		}
		return out[i].Label < out[j].Label
	})
	sorted := make([]Item, len(out))
	for i, j := range idx {
		sorted[i] = out[j]
	}
	return sorted
}

// ASCIIMarker is the selection marker a Mono terminal sees, exported for
// testkit's glyph-parity check against style's GlyphSet.
const ASCIIMarker = "|"

// Glyphs; the ASCII fallback is the caller's Caps decision passed into Render.
const (
	markerUnicode = "▌"
	markerASCII   = ASCIIMarker

	ellipsisUnicode = "…"
	ellipsisASCII   = "..."

	upUnicode = "↑" // footer's "more above" marker
	upASCII   = "^"

	markerGap = " " // keeps unselected rows' Label aligned with selected ones
	indent    = "  "

	detailGap = "  " // separates a Label from its Detail
)

// defaultPageSize is PageMove's step when the caller sets none.
const defaultPageSize = 10

// defaultEmptyText is the empty-state line when the caller sets none.
const defaultEmptyText = "no matches"

// Picker holds an item set, the current query's filtered view of it, and the
// selection. The zero value is not usable — construct with New.
type Picker struct {
	items     []Item
	filtered  []Item
	query     string
	sel       int
	pageSize  int
	emptyText string
	header    string
}

// New returns an empty Picker with no query and the default empty text.
func New() *Picker {
	return &Picker{pageSize: defaultPageSize, emptyText: defaultEmptyText}
}

// SetItems replaces the item set, re-applies the current query, and resets
// the selection to the top. Label and Detail are sanitized here, once; ID is
// left alone since it is never rendered.
func (p *Picker) SetItems(items []Item) {
	clean := make([]Item, len(items))
	for i, it := range items {
		it.Label = sanitize.Line(it.Label)
		it.Detail = sanitize.Line(it.Detail)
		clean[i] = it
	}
	p.items = clean
	p.refilter()
}

// SetQuery sets the filter query and resets the selection to the top (the
// best match under the new query).
func (p *Picker) SetQuery(q string) {
	p.query = q
	p.refilter()
}

// Query returns the current query.
func (p *Picker) Query() string { return p.query }

// SetEmptyText sets the single muted line rendered when nothing matches.
func (p *Picker) SetEmptyText(s string) { p.emptyText = s }

// SetHeader sets a single muted context line drawn as the overlay's first
// row; "" clears it. Sanitized like Label and Detail. Lives inside the
// maxRows budget, like the footer, and is dropped when the budget is a
// single row.
func (p *Picker) SetHeader(s string) { p.header = sanitize.Line(s) }

// Header returns the current header text, "" when there is none.
func (p *Picker) Header() string { return p.header }

// SetPageSize sets PageMove's step. Non-positive values are ignored.
func (p *Picker) SetPageSize(n int) {
	if n > 0 {
		p.pageSize = n
	}
}

func (p *Picker) refilter() {
	p.filtered = Filter(p.items, p.query)
	p.sel = 0
}

// Len returns the total number of items, before filtering.
func (p *Picker) Len() int { return len(p.items) }

// FilteredLen returns the number of items matching the current query — the
// count the "+N more" footer is computed against.
func (p *Picker) FilteredLen() int { return len(p.filtered) }

// Move changes the selection by delta rows, clamped (never wrapped) to the
// filtered list.
func (p *Picker) Move(delta int) {
	if len(p.filtered) == 0 {
		p.sel = 0
		return
	}
	p.sel += delta
	if p.sel < 0 {
		p.sel = 0
	}
	if p.sel > len(p.filtered)-1 {
		p.sel = len(p.filtered) - 1
	}
}

// PageMove moves the selection by delta pages (see SetPageSize).
func (p *Picker) PageMove(delta int) { p.Move(delta * p.pageSize) }

// SelectedIndex returns the selection's index within the filtered list, or
// -1 when nothing matches.
func (p *Picker) SelectedIndex() int {
	if len(p.filtered) == 0 {
		return -1
	}
	return p.sel
}

// Selected returns the currently selected item. ok is false when the
// filtered list is empty, which is the caller's signal that Enter must do
// nothing.
func (p *Picker) Selected() (Item, bool) {
	if len(p.filtered) == 0 {
		return Item{}, false
	}
	return p.filtered[p.sel], true
}

// Render draws the overlay: one row per visible item, windowed around the
// selection, never more than maxRows lines and never wider than width. When
// more items match than fit, the last line is a muted footer reporting both
// scroll directions ("↑N above", "+N more"), inside the budget rather than
// on top of it. With nothing matching, the result is the empty-state line
// (see SetEmptyText). ascii must match the caller's caps profile. A header
// (see SetHeader) takes the first line and one row of the budget.
func (p *Picker) Render(width, maxRows int, ascii bool) []frame.Line {
	if width <= 0 || maxRows <= 0 {
		return nil
	}
	if p.header != "" && maxRows > 1 {
		head := frame.Styled(frame.StyleMuted, clip(p.header, width, ascii))
		return append([]frame.Line{head}, p.body(width, maxRows-1, ascii)...)
	}
	return p.body(width, maxRows, ascii)
}

// body renders the rows themselves: everything Render promises except the
// header, against the budget left over once the header has taken its row.
func (p *Picker) body(width, maxRows int, ascii bool) []frame.Line {
	n := len(p.filtered)
	if n == 0 {
		return []frame.Line{frame.Styled(frame.StyleMuted, clip(p.emptyText, width, ascii))}
	}

	rows := maxRows
	footer := false
	if n > maxRows {
		footer = true
		rows = maxRows - 1
		if rows < 1 {
			// A footer alone says nothing useful; show the item instead.
			rows = 1
			footer = false
		}
	}
	if rows > n {
		rows = n
	}

	// Centre the window on the selection, then clamp to the list bounds; the
	// window is derived, not stored.
	start := p.sel - rows/2
	if max := n - rows; start > max {
		start = max
	}
	if start < 0 {
		start = 0
	}

	lines := make([]frame.Line, 0, rows+1)
	for i := start; i < start+rows; i++ {
		lines = append(lines, p.row(p.filtered[i], i == p.sel, width, ascii))
	}
	if footer {
		text := footerText(start, n-(start+rows), ascii)
		lines = append(lines, frame.Styled(frame.StyleMuted, clip(text, width, ascii)))
	}
	return lines
}

// footerText is the hidden-rows note, indented to line up with the labels
// it counts.
func footerText(above, below int, ascii bool) string {
	var parts []string
	if above > 0 {
		parts = append(parts, upMarker(ascii)+itoa(above)+" above")
	}
	if below > 0 {
		parts = append(parts, "+"+itoa(below)+" more")
	}
	return indent + strings.Join(parts, "  ")
}

func upMarker(ascii bool) string {
	if ascii {
		return upASCII
	}
	return upUnicode
}

// row renders one item to at most width cells.
func (p *Picker) row(it Item, selected bool, width int, ascii bool) frame.Line {
	prefixWidth := textwidth.Width(indent)
	budget := width - prefixWidth

	var line frame.Line
	switch {
	case width < prefixWidth:
		// Narrower than the gutter itself; keep the marker if one cell is
		// all there is.
		if selected {
			line = frame.Line{frame.S(frame.StyleBrand, textwidth.Truncate(marker(ascii), width, ""))}
		} else {
			line = frame.Line{frame.S(frame.StyleNone, strings.Repeat(" ", width))}
		}
	case selected:
		line = frame.Line{
			frame.S(frame.StyleBrand, marker(ascii)),
			frame.S(frame.StyleNone, markerGap),
		}
	default:
		line = frame.Line{frame.S(frame.StyleNone, indent)}
	}
	if budget <= 0 {
		return line
	}

	label, detail := it.Label, it.Detail
	if textwidth.Width(label) > budget {
		// A truncated label has spent the row; drop Detail rather than a
		// sliver of it.
		label = textwidth.Truncate(label, budget, fitTail(budget, ascii))
		detail = ""
	} else if detail != "" {
		rem := budget - textwidth.Width(label) - textwidth.Width(detailGap)
		if rem >= 2 {
			detail = textwidth.Truncate(detail, rem, fitTail(rem, ascii))
		} else {
			detail = ""
		}
	}

	labelStyle := frame.StyleNone
	if selected {
		labelStyle = frame.StyleActive
	}
	line = append(line, frame.S(labelStyle, label))
	if detail != "" {
		line = append(line,
			frame.S(frame.StyleNone, detailGap),
			frame.S(frame.StyleMuted, detail))
	}
	return line
}

func marker(ascii bool) string {
	if ascii {
		return markerASCII
	}
	return markerUnicode
}

func ellipsis(ascii bool) string {
	if ascii {
		return ellipsisASCII
	}
	return ellipsisUnicode
}

// fitTail is the elision marker for a budget, or "" when the marker itself
// does not fit. This guard matters: textwidth.Truncate handed a tail wider
// than its budget returns the bare tail, which would overflow the row.
func fitTail(budget int, ascii bool) string {
	tail := ellipsis(ascii)
	if textwidth.Width(tail) > budget {
		return ""
	}
	return tail
}

// clip truncates s to width cells with the right ellipsis for the profile.
func clip(s string, width int, ascii bool) string {
	if textwidth.Width(s) <= width {
		return s
	}
	return textwidth.Truncate(s, width, fitTail(width, ascii))
}

// itoa avoids pulling strconv in for one non-negative footer count.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

// Package picker is beam's generic keyboard list overlay: the one filtered,
// ranked, windowed list every "choose one of these" surface renders through.
// The @-mention file list (file-addressing, blueprint 4.12) and the session
// picker (session-manager, blueprint 4.8 "Later: real picker overlay") are
// the two MVP callers; the command palette can adopt it unchanged.
//
// It is pure: a Picker holds items, a query, and a selection, and Render is a
// function of (state, width, maxRows) -> []frame.Line. No I/O, no services,
// no terminal reads. Candidate SOURCING belongs to the caller — the file
// list comes from comp/fileaddr (which rides vfs), the session list from
// engine-bridge — so this package never learns what a path or a session is.
//
// Ranking lives here too (Rank/Filter) rather than in each caller, because
// the blueprint pins one order — basename-prefix, then path-substring, then
// fuzzy subsequence — and two implementations of it would drift. Inside that
// third tier, where the pinned order says nothing and a plain alphabetical
// sort is arbitrary, [FuzzyScore] decides.
package picker

import (
	"path"
	"sort"
	"strings"

	"github.com/contenox/beam/internal/surfaces/beamtui/frame"
	"github.com/contenox/beam/internal/surfaces/beamtui/sanitize"
	"github.com/contenox/beam/internal/surfaces/beamtui/textwidth"
)

// Item is one selectable row. ID is the caller's stable identity (a resolved
// absolute path, a session id) and is never rendered; Label is the primary
// text and the string ranking matches against; Detail is the dimmed
// right-hand context (a parent directory, a session's message count).
//
// Rank is an OUTPUT: Filter/SetQuery overwrite it with the match tier the
// query produced. Callers may leave it zero.
type Item struct {
	ID     string
	Label  string
	Detail string
	Rank   int
}

// Match tiers, best first. The order is the blueprint's (4.12): an exact
// basename prefix beats a substring anywhere in the path, which beats a
// fuzzy subsequence.
const (
	// RankBasenamePrefix: the query is a prefix of the label's last
	// slash-separated segment.
	RankBasenamePrefix = 0
	// RankPathSubstring: the query appears contiguously anywhere in the label.
	RankPathSubstring = 1
	// RankSubsequence: the query's runes appear in the label in order, with
	// gaps.
	RankSubsequence = 2
	// RankAny is the tier every item gets under an empty query, where there
	// is nothing to rank by and caller order is authoritative.
	RankAny = 3
)

// Rank returns the match tier of label under query, and whether it matched at
// all. Matching is case-insensitive. An empty query matches everything at
// RankAny. Labels are treated as slash-separated paths for the basename
// tier; a label with no slash is its own basename, so plain names (session
// titles, command names) rank correctly with no special casing.
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
// slice sorted by tier then Label. It is the single ranking implementation:
// SetQuery uses it, and comp/fileaddr uses it, so the human's @-list and any
// other picker order identically.
//
// An empty query is the deliberate exception: nothing is dropped, nothing is
// reordered, and the caller's own order (a session roster's recency, a walk's
// lexical order) survives. Sorting by Label there would silently impose an
// alphabetical roster on callers that had a better order.
//
// Within a tier the order is by Label, with ONE exception: the fuzzy
// subsequence tier is ordered by [FuzzyScore], best first, and falls back to
// Label only for a genuine tie. That tier is where the pinned order runs out
// — "kbd" matches keybindings.go and workbench_dashboard.go equally as far as
// the tiers are concerned — so alphabetical there is not a neutral default,
// it is a coin flip dressed as a rule. The two stronger tiers keep their
// alphabetical order untouched, because a basename prefix or a contiguous
// path substring already IS the ranking evidence and re-scoring it would
// reorder lists that are currently correct.
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
	// Scores are computed ONCE per item rather than inside the comparator: a
	// sort calls its less function O(n log n) times, and FuzzyScore is a DP
	// over the whole label.
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
	// Stable so items that tie on (tier, score, Label) — legal, since Label
	// is not required to be unique — keep their input order.
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

// ASCIIMarker is the selection marker a Mono terminal sees, exported so
// testkit's glyph-parity test can hold every surface's ASCII beam-bar against
// the style package's GlyphSet in one place. Components may not import style,
// so the agreement can only be checked from outside.
const ASCIIMarker = "|"

// Glyphs. The marker is the beam-bar, the same device comp/brand hangs the
// welcome header off and the composer uses as its sigil; the ASCII fallback
// is the caller's Caps decision, passed into Render (this package never
// probes a terminal).
const (
	markerUnicode = "▌"
	markerASCII   = ASCIIMarker

	ellipsisUnicode = "…"
	ellipsisASCII   = "..."

	// upUnicode/upASCII mark the footer's "there is more above" count. The
	// caret is the same one comp/palette and comp/composer use, so one glyph
	// means "scrolled past" everywhere in the live region.
	upUnicode = "↑"
	upASCII   = "^"

	// indent is what unselected rows carry in place of "marker + markerGap",
	// so every Label starts in the same column whether or not it is selected.
	markerGap = " "
	indent    = "  "

	// detailGap separates a Label from its Detail.
	detailGap = "  "
)

// defaultPageSize is PageMove's step when the caller sets none. It is a
// keyboard convenience, not a layout fact: Render is told its own row budget
// per call and holds no scroll state, so there is no window height here to
// derive a page from.
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
// the selection to the top. Callers refreshing a live candidate list (every
// debounced keystroke, for @-mentions) call this; the selection deliberately
// does not chase an item across refreshes, because the list under it has
// changed meaning.
//
// Label and Detail are SANITIZED here, once, rather than at every render. A
// picker's rows are the most casually-sourced strings in the TUI — a file
// name out of the workspace, a session title somebody else set — and a
// filename is allowed to contain very nearly anything the filesystem will
// take, escape sequences included. ID is left alone: it is the caller's
// identity token and is never rendered, so mangling it would break the
// splice that follows the selection while protecting nothing.
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
// File-addressing uses it for the blueprint's fixed no-root state.
func (p *Picker) SetEmptyText(s string) { p.emptyText = s }

// SetHeader sets a single muted context line drawn as the overlay's FIRST
// row; "" (the default) clears it and the overlay renders exactly as it did
// before headers existed. The text is sanitized here, once, like Label and
// Detail — a header's natural source is a filesystem path.
//
// The header lives INSIDE the maxRows budget, the same contract the "+N more"
// footer has: a caller that reserved maxRows lines of its live region gets at
// most maxRows lines back, header and footer included. Two overlay decorations
// with two different answers to "does this cost me a row" is how a composer
// gets pushed off the bottom of the terminal.
//
// It is dropped when the budget is a single row: a header with nothing under
// it names a list the caller cannot see, and the one available line is better
// spent on the selected item (or on the empty-state line) than on its title.
//
// The intended feed is [fileaddr.Browser.Breadcrumb] — the directory the
// @-mention browser is currently listing, which is otherwise invisible once
// the rows are full root-relative paths.
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

// Move changes the selection by delta rows, clamped to the filtered list.
// Clamping rather than wrapping is deliberate: holding a key at either end
// of a candidate list should stop there, not silently teleport the selection
// to the opposite end of a list the user is still reading.
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
// selection, never more than maxRows lines total and never wider than width.
//
// The selected row is marked with the gold beam-bar and its Label carries
// StyleActive; unselected rows are indented to the same column. When more
// items match than fit, the LAST of the maxRows lines is a muted footer
// (blueprint 4.12: capped, with a footer) — inside the budget, not on top of
// it, so a caller that reserved maxRows rows gets exactly that many.
//
// The footer reports BOTH directions — "↑N above" when the window has
// scrolled past the top, "+N more" when items remain below it, and both when
// the window is in the middle of a long list. Each count is what carrying on
// in that direction would reach, which is the only reading that makes the
// numbers true: one combined "hidden" count made a scrolled list claim there
// were five more below when three of them were above.
//
// It used to count only what was below, and hand its line back to an item once
// the window reached the end — which is precisely the moment the operator most
// needs to be told the list continues upward, because at the bottom of a
// scrolled list the rows above are the ONLY hidden ones and nothing on screen
// said they existed. So the footer now stands whenever anything at all is
// hidden, and it is indented to the row column so it reads as a note about the
// list rather than as another row of it.
//
// With nothing matching, the result is the single muted empty-state line
// (see SetEmptyText). ascii selects the character fallbacks and must be true
// exactly when the caller's caps profile is Mono.
//
// A header (see SetHeader) takes the first line and one row of the budget,
// leaving the rest of this arithmetic unchanged.
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
			// A footer with no rows above it says nothing useful; spend the
			// single available line on the selected item instead.
			rows = 1
			footer = false
		}
	}
	if rows > n {
		rows = n
	}

	// Centre the window on the selection, then clamp to the list bounds. The
	// window is derived, not stored, so a Picker has no scroll state that can
	// disagree with its selection.
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

// footerText is the hidden-rows note: what the window has scrolled past, what
// it has not reached yet, or both. It carries the same indent an unselected
// row does, so the note lines up under the labels it is counting.
//
// It is never empty where it is called from: the footer only exists when more
// items match than fit, and a window that hides nothing below it has by
// definition scrolled past something above.
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
		// Narrower than the gutter itself. Keep the marker if a single cell
		// is all there is — a selection the user cannot see is worse than a
		// row with no text — and never exceed width.
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
		// A truncated label has already spent the row; a Detail beside it
		// would be a sliver of context attached to an incomplete path.
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
// does not fit.
//
// This guard is load-bearing, not defensive: textwidth.Truncate handed a tail
// wider than its budget returns THE BARE TAIL, so an ASCII picker three cells
// short of its "..." would emit a row wider than the terminal to announce
// that it had cut something. Below the marker's own width the honest render
// is as much of the text as fits and no marker at all.
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

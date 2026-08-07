package term

import (
	"strconv"
	"strings"
	"unicode/utf8"
)

// vt is the smallest terminal the painter can be judged against: what the
// SCREEN ends up looking like, rather than which bytes were emitted to make it
// look that way. Byte-exact assertions are what let two broken resize
// implementations ship — they pin the algorithm, and the algorithm was the bug.
//
// It models the terminal beam actually runs in: no alternate screen, so rows
// are an unbounded buffer that only ever grows downward and ED0 is "erase to
// the end of what has been painted"; DECAWM auto-wrap with deferred wrap, so
// filling the last cell parks the cursor without consuming a row; and — the
// property the whole bug lives in — reflow, so resize rejoins soft-wrapped
// logical lines and rewraps them at the new width. Only the sequences the
// painter is allowed to emit are honored; private modes are parsed and ignored,
// since synchronized output and cursor visibility have no effect on content.
type vt struct {
	width       int
	rows        []vtRow
	row, col    int
	pendingWrap bool
}

// vtRow is one physical row. wrapped marks that the row below is the
// continuation of this one rather than a new logical line, which is the flag a
// reflowing terminal rejoins on; a line feed and an erase both clear it,
// because neither leaves anything for the next row to continue.
type vtRow struct {
	cells   []rune
	wrapped bool
}

func newScreen(width int) *vt { return &vt{width: width} }

// Write lets the screen stand in for the painter's terminal directly, so tests
// drive the real commit path rather than a transcript of it.
func (v *vt) Write(p []byte) (int, error) {
	s := string(p)
	for i := 0; i < len(s); {
		switch c := s[i]; {
		case c == '\r':
			v.col, v.pendingWrap = 0, false
			i++
		case c == '\n':
			v.lineFeed()
			i++
		case c == 0x1b:
			i += v.escape(s[i:])
		default:
			r, size := utf8.DecodeRuneInString(s[i:])
			v.put(r)
			i += size
		}
	}
	return len(p), nil
}

// escape consumes one CSI sequence and reports its length. A lone ESC is
// consumed as a single byte: the painter never emits one, and spanText exists
// precisely so none can reach here.
func (v *vt) escape(s string) int {
	if len(s) < 2 || s[1] != '[' {
		return 1
	}
	j := 2
	for j < len(s) && (s[j] == '?' || s[j] == ';' || (s[j] >= '0' && s[j] <= '9')) {
		j++
	}
	if j >= len(s) {
		return len(s)
	}
	params, final := s[2:j], s[j]
	j++
	if strings.HasPrefix(params, "?") {
		return j // ?2026 sync, ?25 cursor, ?2004 paste, ?1004 focus: no content
	}
	n := 1
	if params != "" {
		n, _ = strconv.Atoi(params)
	}
	switch final {
	case 'A':
		v.moveRow(-n)
	case 'B':
		v.moveRow(n)
	case 'C':
		v.moveCol(n)
	case 'K':
		if params == "2" {
			v.eraseLine()
		}
	case 'J':
		if params == "" || params == "0" {
			v.eraseBelow()
		}
	}
	return j
}

func (v *vt) ensure(row int) {
	for len(v.rows) <= row {
		v.rows = append(v.rows, vtRow{cells: blankCells(v.width)})
	}
}

func (v *vt) put(r rune) {
	if v.pendingWrap {
		v.ensure(v.row)
		v.rows[v.row].wrapped = true
		v.row++
		v.col, v.pendingWrap = 0, false
	}
	v.ensure(v.row)
	v.rows[v.row].cells[v.col] = r
	if v.col == v.width-1 {
		v.pendingWrap = true // deferred: only the next printable rune wraps
	} else {
		v.col++
	}
}

func (v *vt) lineFeed() {
	v.ensure(v.row)
	v.rows[v.row].wrapped = false
	v.row++
	v.pendingWrap = false
	v.ensure(v.row)
}

func (v *vt) moveRow(delta int) {
	v.pendingWrap = false
	v.row += delta
	if v.row < 0 {
		v.row = 0
	}
	v.ensure(v.row)
}

func (v *vt) moveCol(delta int) {
	v.pendingWrap = false
	v.col += delta
	if v.col > v.width-1 {
		v.col = v.width - 1
	}
	if v.col < 0 {
		v.col = 0
	}
}

func (v *vt) eraseLine() {
	v.ensure(v.row)
	v.rows[v.row] = vtRow{cells: blankCells(v.width)}
	v.pendingWrap = false
}

// eraseBelow is ED0: the rest of the current row plus everything under it.
// Dropping the rows below rather than blanking them is the same screen, and
// keeps the buffer's length meaningful as "how far painting has reached".
func (v *vt) eraseBelow() {
	v.ensure(v.row)
	for i := v.col; i < v.width; i++ {
		v.rows[v.row].cells[i] = ' '
	}
	v.rows[v.row].wrapped = false
	v.rows = v.rows[:v.row+1]
	v.pendingWrap = false
}

// resize rewraps the screen the way a reflowing terminal does: soft-wrapped
// runs are rejoined into logical lines and re-broken at the new width, and the
// cursor is carried along by its offset within its logical line. This is the
// behavior that makes the painter's row counts lie, so a test that does not
// model it cannot see the defect at all.
func (v *vt) resize(w int) {
	if w <= 0 || w == v.width {
		return
	}
	var (
		logs     [][]rune
		cur      []rune
		curLine  = 0
		curOff   = 0
		haveCurs = false
	)
	for i := range v.rows {
		start := len(cur)
		cur = append(cur, v.rows[i].cells...)
		if i == v.row && !haveCurs {
			curLine, curOff, haveCurs = len(logs), start+v.col, true
		}
		if v.rows[i].wrapped && i+1 < len(v.rows) {
			continue
		}
		logs = append(logs, cur)
		cur = nil
	}
	if cur != nil {
		logs = append(logs, cur)
	}

	v.width = w
	rows := make([]vtRow, 0, len(logs))
	newRow, newCol := 0, 0
	for li, text := range logs {
		start := len(rows)
		segs := chunkCells(trimTrailing(text), w)
		for si, seg := range segs {
			rows = append(rows, vtRow{cells: padCells(seg, w), wrapped: si < len(segs)-1})
		}
		if li != curLine {
			continue
		}
		if r := start + curOff/w; r < len(rows) {
			newRow, newCol = r, curOff%w
		} else { // the cursor sat in trailing blanks the rejoin dropped
			newRow, newCol = len(rows)-1, 0
		}
	}
	v.rows = rows
	if newRow >= len(rows) {
		newRow = max(len(rows)-1, 0)
	}
	v.row, v.col, v.pendingWrap = newRow, newCol, false
}

// text renders the screen as the user sees it: trailing blanks stripped, one
// line per physical row, trailing blank rows dropped.
func (v *vt) text() string {
	out := make([]string, 0, len(v.rows))
	for _, r := range v.rows {
		out = append(out, strings.TrimRight(string(r.cells), " "))
	}
	for len(out) > 0 && out[len(out)-1] == "" {
		out = out[:len(out)-1]
	}
	return strings.Join(out, "\n")
}

// logical returns the screen's logical lines: physical rows rejoined wherever
// the terminal soft-wrapped one. This is the unit the user perceives as a line
// and the unit a selection copies, so it is what "did this line survive the
// resize unchanged" has to be asked in — a narrow terminal wraps committed
// history without altering it.
func (v *vt) logical() []string {
	var out []string
	var cur strings.Builder
	for i, r := range v.rows {
		cur.WriteString(string(r.cells))
		if r.wrapped && i+1 < len(v.rows) {
			continue
		}
		out = append(out, strings.TrimRight(cur.String(), " "))
		cur.Reset()
	}
	for len(out) > 0 && out[len(out)-1] == "" {
		out = out[:len(out)-1]
	}
	return out
}

// count counts occurrences across logical lines, so a mark is not missed
// merely because the terminal wrapped it.
func (v *vt) count(sub string) int {
	return strings.Count(strings.Join(v.logical(), "\n"), sub)
}

func blankCells(w int) []rune {
	cells := make([]rune, w)
	for i := range cells {
		cells[i] = ' '
	}
	return cells
}

func padCells(seg []rune, w int) []rune {
	cells := blankCells(w)
	copy(cells, seg)
	return cells
}

func trimTrailing(text []rune) []rune {
	end := len(text)
	for end > 0 && text[end-1] == ' ' {
		end--
	}
	return text[:end]
}

// chunkCells breaks a logical line into physical rows, always at least one so
// an empty line still occupies a row.
func chunkCells(text []rune, w int) [][]rune {
	if len(text) == 0 {
		return [][]rune{nil}
	}
	var segs [][]rune
	for i := 0; i < len(text); i += w {
		segs = append(segs, text[i:min(i+w, len(text))])
	}
	return segs
}

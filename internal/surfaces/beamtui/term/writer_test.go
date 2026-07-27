package term

import (
	"bytes"
	"errors"
	"regexp"
	"strings"
	"testing"

	"github.com/contenox/beam/internal/surfaces/beamtui/frame"
	"github.com/contenox/beam/internal/surfaces/beamtui/textwidth"
)

// flakyWriter counts writes and can fail one of them halfway through, which
// is what a tty does when the terminal goes away or the pipe fills mid-frame.
type flakyWriter struct {
	buf    bytes.Buffer
	writes int
	failOn int // 1-based index of the write that fails; 0 never fails
	err    error
}

func (w *flakyWriter) Write(p []byte) (int, error) {
	w.writes++
	if w.failOn != 0 && w.writes == w.failOn {
		n := len(p) / 2
		w.buf.Write(p[:n]) // a partial frame reached the screen
		return n, w.err
	}
	w.buf.Write(p)
	return len(p), nil
}

// tagStyles is a resolver whose "SGR" codes are readable tags, so tests can
// assert that every span went through style resolution and in what order.
type tagStyles struct{ calls []frame.StyleID }

func (t *tagStyles) SGR(id frame.StyleID) (string, string) {
	t.calls = append(t.calls, id)
	if id == frame.StyleNone {
		return "", ""
	}
	return "<" + string(id) + ">", "</" + string(id) + ">"
}

type plainStyles struct{}

func (plainStyles) SGR(frame.StyleID) (string, string) { return "", "" }

func newTestPainter(width, height int) (*painter, *bytes.Buffer) {
	var buf bytes.Buffer
	return &painter{out: &buf, styles: plainStyles{}, width: width, height: height}, &buf
}

func commit(t *testing.T, p *painter, buf *bytes.Buffer, f frame.Frame) string {
	t.Helper()
	buf.Reset()
	if err := p.commit(f); err != nil {
		t.Fatalf("commit: %v", err)
	}
	return buf.String()
}

func lines(texts ...string) []frame.Line {
	out := make([]frame.Line, 0, len(texts))
	for _, s := range texts {
		out = append(out, frame.Plain(s))
	}
	return out
}

var escapes = regexp.MustCompile(`\x1b\[[0-9;?]*[a-zA-Z]|\x1b[]P][^\x07\x1b]*(\x07|\x1b\\)?`)

// visible strips escape sequences, leaving what the terminal would show.
func visible(s string) string { return escapes.ReplaceAllString(s, "") }

func TestUnit_CommitFirstPaintWritesEveryRow(t *testing.T) {
	p, buf := newTestPainter(20, 10)
	got := commit(t, p, buf, frame.Frame{
		Live:   lines("one", "two", "three"),
		Cursor: frame.Cursor{Row: 2, Col: 3},
	})
	want := seqSyncBegin + seqCursorHide +
		"\r" + seqClearLine + "one" +
		"\r\n\r" + seqClearLine + "two" +
		"\r\n\r" + seqClearLine + "three" +
		"\r" + cursorRight(3) + seqCursorShow +
		seqSyncEnd
	if got != want {
		t.Fatalf("first commit\n got %q\nwant %q", got, want)
	}
}

func TestUnit_CommitUnchangedFrameWritesNothing(t *testing.T) {
	p, buf := newTestPainter(20, 10)
	f := frame.Frame{Live: lines("one", "two"), Cursor: frame.Cursor{Row: 1, Col: 1}}
	commit(t, p, buf, f)
	if got := commit(t, p, buf, f); got != "" {
		t.Fatalf("recommit of an identical frame wrote %q, want no bytes", got)
	}
}

func TestUnit_CommitSingleRowChangeRepaintsOnlyThatRow(t *testing.T) {
	p, buf := newTestPainter(20, 10)
	commit(t, p, buf, frame.Frame{
		Live:   lines("one", "two", "three"),
		Cursor: frame.Cursor{Row: 2, Col: 3},
	})
	got := commit(t, p, buf, frame.Frame{
		Live:   lines("one", "TWO", "three"),
		Cursor: frame.Cursor{Row: 2, Col: 3},
	})
	want := seqSyncBegin + seqCursorHide +
		cursorUp(1) + "\r" + seqClearLine + "TWO" +
		cursorDown(1) + "\r" + cursorRight(3) + seqCursorShow +
		seqSyncEnd
	if got != want {
		t.Fatalf("single-row change\n got %q\nwant %q", got, want)
	}
	if n := strings.Count(got, seqClearLine); n != 1 {
		t.Fatalf("cleared %d rows, want exactly the changed one", n)
	}
	if strings.Contains(got, "one") || strings.Contains(got, "three") {
		t.Fatalf("unchanged rows were repainted: %q", got)
	}
}

func TestUnit_CommitCursorOnlyChangeMovesWithoutRepaint(t *testing.T) {
	p, buf := newTestPainter(20, 10)
	live := lines("one", "two")
	commit(t, p, buf, frame.Frame{Live: live, Cursor: frame.Cursor{Row: 0, Col: 0}})
	got := commit(t, p, buf, frame.Frame{Live: live, Cursor: frame.Cursor{Row: 1, Col: 2}})
	if strings.Contains(got, seqClearLine) {
		t.Fatalf("cursor move repainted a row: %q", got)
	}
	want := seqSyncBegin + seqCursorHide +
		cursorDown(1) + "\r" + cursorRight(2) + seqCursorShow + seqSyncEnd
	if got != want {
		t.Fatalf("cursor-only commit\n got %q\nwant %q", got, want)
	}
}

func TestUnit_CommitScrollbackPrintsOnceInOrderAboveLive(t *testing.T) {
	p, buf := newTestPainter(20, 10)
	live := lines("composer", "status")
	commit(t, p, buf, frame.Frame{Live: live, Cursor: frame.Cursor{Row: 0}})

	got := commit(t, p, buf, frame.Frame{
		Scrollback: lines("hist one", "hist two"),
		Live:       live,
		Cursor:     frame.Cursor{Row: 0},
	})
	first, second := strings.Index(got, "hist one"), strings.Index(got, "hist two")
	if first < 0 || second < 0 || second < first {
		t.Fatalf("scrollback out of order or missing: %q", got)
	}
	if live := strings.Index(got, "composer"); live < second {
		t.Fatalf("live region painted before scrollback: %q", got)
	}
	if n := strings.Count(got, "hist one"); n != 1 {
		t.Fatalf("scrollback line written %d times, want once", n)
	}
	if !strings.Contains(got, cursorUp(1)) {
		t.Fatalf("scrollback did not rewind to the top of the live region: %q", got)
	}

	again := commit(t, p, buf, frame.Frame{Live: live, Cursor: frame.Cursor{Row: 0}})
	if strings.Contains(again, "hist") {
		t.Fatalf("scrollback repainted on a later commit: %q", again)
	}
	if again != "" {
		t.Fatalf("idle commit after scrollback wrote %q, want no bytes", again)
	}
}

// A commit carrying scrollback cannot know how far printing scrolls, so it
// blanks every row the old live region owned BEFORE printing rather than
// hunting vacated rows afterwards. Nothing stale can survive that, whatever
// the terminal does with an over-wide history line.
func TestUnit_CommitScrollbackBlanksTheWholePreviousLiveRegionFirst(t *testing.T) {
	p, buf := newTestPainter(20, 10)
	commit(t, p, buf, frame.Frame{Live: lines("a", "b", "c", "d"), Cursor: frame.Cursor{Row: 0}})
	got := commit(t, p, buf, frame.Frame{
		Scrollback: lines("hist"),
		Live:       lines("a", "b"),
		Cursor:     frame.Cursor{Row: 0},
	})
	blank := "\r" + seqClearLine +
		cursorDown(1) + "\r" + seqClearLine +
		cursorDown(1) + "\r" + seqClearLine +
		cursorDown(1) + "\r" + seqClearLine +
		cursorUp(3)
	want := seqSyncBegin + seqCursorHide + blank +
		"\r" + seqClearLine + "hist" + "\r\n" +
		"\r" + seqClearLine + "a" +
		"\r\n\r" + seqClearLine + "b" +
		cursorUp(1) + "\r" + seqCursorShow +
		seqSyncEnd
	if got != want {
		t.Fatalf("scrollback commit\n got %q\nwant %q", got, want)
	}
	head, _, _ := strings.Cut(got, "hist")
	if n := strings.Count(head, seqClearLine); n != 5 {
		t.Fatalf("blanked %d rows before printing history, want the 4 previous live rows plus the history row", n)
	}
}

// The live region growing in the same commit that carries scrollback: it is
// re-anchored below the printed history and painted fresh at the new height,
// with no vacated-row arithmetic left over from the old one.
func TestUnit_CommitScrollbackWithGrowingLiveRegion(t *testing.T) {
	p, buf := newTestPainter(20, 10)
	commit(t, p, buf, frame.Frame{Live: lines("a"), Cursor: frame.Cursor{Row: 0}})
	got := commit(t, p, buf, frame.Frame{
		Scrollback: lines("hist"),
		Live:       lines("a", "b", "c"),
		Cursor:     frame.Cursor{Row: 2},
	})
	want := seqSyncBegin + seqCursorHide +
		"\r" + seqClearLine + // the single previous live row, blanked
		"\r" + seqClearLine + "hist" + "\r\n" +
		"\r" + seqClearLine + "a" +
		"\r\n\r" + seqClearLine + "b" +
		"\r\n\r" + seqClearLine + "c" +
		"\r" + seqCursorShow +
		seqSyncEnd
	if got != want {
		t.Fatalf("scrollback with a growing live region\n got %q\nwant %q", got, want)
	}
	if strings.Contains(got, cursorDown(1)) {
		t.Fatalf("scrollback path cleared rows after painting; blanking happens up front: %q", got)
	}
}

// A live region taller than the terminal shows its tail, so it owns exactly
// the terminal's height in rows — the blanking pass reclaims those and no
// more, and can never walk off the bottom of the screen. The frame's cursor
// row (4) is remapped by the two rows the clamp dropped and lands on the last
// visible row; see TestUnit_CommitClampedLiveRemapsTheCursorRow for the cases
// where remapping and clamping disagree.
func TestUnit_CommitScrollbackWithLiveTallerThanTerminal(t *testing.T) {
	p, buf := newTestPainter(20, 3)
	live := lines("a", "b", "c", "d", "e")
	commit(t, p, buf, frame.Frame{Live: live, Cursor: frame.Cursor{Row: 4}})
	got := commit(t, p, buf, frame.Frame{
		Scrollback: lines("hist"),
		Live:       live,
		Cursor:     frame.Cursor{Row: 4},
	})
	want := seqSyncBegin + seqCursorHide +
		cursorUp(2) + // back to the top of the region
		"\r" + seqClearLine +
		cursorDown(1) + "\r" + seqClearLine +
		cursorDown(1) + "\r" + seqClearLine +
		cursorUp(2) +
		"\r" + seqClearLine + "hist" + "\r\n" +
		"\r" + seqClearLine + "c" +
		"\r\n\r" + seqClearLine + "d" +
		"\r\n\r" + seqClearLine + "e" +
		"\r" + seqCursorShow +
		seqSyncEnd
	if got != want {
		t.Fatalf("scrollback under a too-tall live region\n got %q\nwant %q", got, want)
	}
}

// A commit with scrollback and no live region at all still leaves the region
// owning the one blanked row it parked on, so the next scrollback commit
// reclaims it.
func TestUnit_CommitScrollbackWithEmptyLiveRegion(t *testing.T) {
	p, buf := newTestPainter(20, 10)
	commit(t, p, buf, frame.Frame{Live: lines("a", "b"), Cursor: frame.Cursor{Row: 0}})
	got := commit(t, p, buf, frame.Frame{
		Scrollback: lines("hist"),
		Cursor:     frame.Cursor{Hidden: true},
	})
	want := seqSyncBegin + seqCursorHide +
		"\r" + seqClearLine +
		cursorDown(1) + "\r" + seqClearLine +
		cursorUp(1) +
		"\r" + seqClearLine + "hist" + "\r\n" +
		"\r" + seqClearLine +
		seqSyncEnd
	if got != want {
		t.Fatalf("scrollback with an empty live region\n got %q\nwant %q", got, want)
	}
	again := commit(t, p, buf, frame.Frame{
		Scrollback: lines("more"),
		Live:       lines("x"),
		Cursor:     frame.Cursor{Hidden: true},
	})
	wantAgain := seqSyncBegin + seqCursorHide +
		"\r" + seqClearLine + // the parked row is still owned, and blanked
		"\r" + seqClearLine + "more" + "\r\n" +
		"\r" + seqClearLine + "x" +
		seqSyncEnd
	if again != wantAgain {
		t.Fatalf("scrollback after an empty live region\n got %q\nwant %q", again, wantAgain)
	}
}

// The blanking pass only reclaims rows the painter owns: on the first commit,
// and after reset() hands the screen back, it owns none.
func TestUnit_CommitScrollbackOnAFreshRegionReclaimsNothing(t *testing.T) {
	p, buf := newTestPainter(20, 10)
	got := commit(t, p, buf, frame.Frame{
		Scrollback: lines("hist"),
		Live:       lines("live"),
		Cursor:     frame.Cursor{Hidden: true},
	})
	want := seqSyncBegin + seqCursorHide +
		"\r" + seqClearLine + "hist" + "\r\n" +
		"\r" + seqClearLine + "live" +
		seqSyncEnd
	if got != want {
		t.Fatalf("first commit with scrollback\n got %q\nwant %q", got, want)
	}

	p.reset()
	again := commit(t, p, buf, frame.Frame{
		Scrollback: lines("more"),
		Live:       lines("live"),
		Cursor:     frame.Cursor{Hidden: true},
	})
	if strings.Contains(again, cursorUp(1)) || strings.Contains(again, cursorDown(1)) {
		t.Fatalf("commit after reset moved over rows it no longer owns: %q", again)
	}
}

func TestUnit_CommitHeightShrinkClearsVacatedRows(t *testing.T) {
	p, buf := newTestPainter(20, 10)
	commit(t, p, buf, frame.Frame{Live: lines("a", "b", "c", "d"), Cursor: frame.Cursor{Row: 0}})
	got := commit(t, p, buf, frame.Frame{Live: lines("a", "b"), Cursor: frame.Cursor{Row: 0}})
	want := seqSyncBegin + seqCursorHide +
		cursorDown(1) + // to the last surviving row
		cursorDown(1) + "\r" + seqClearLine +
		cursorDown(1) + "\r" + seqClearLine +
		cursorUp(2) +
		cursorUp(1) + "\r" + seqCursorShow +
		seqSyncEnd
	if got != want {
		t.Fatalf("height shrink\n got %q\nwant %q", got, want)
	}
}

func TestUnit_CommitHeightGrowthAddsRowsWithNewlines(t *testing.T) {
	p, buf := newTestPainter(20, 10)
	commit(t, p, buf, frame.Frame{Live: lines("a", "b"), Cursor: frame.Cursor{Row: 0}})
	got := commit(t, p, buf, frame.Frame{Live: lines("a", "b", "c"), Cursor: frame.Cursor{Row: 0}})
	if !strings.Contains(got, "\r\n\r"+seqClearLine+"c") {
		t.Fatalf("new row was not created with a newline: %q", got)
	}
	if strings.Contains(got, seqClearLine+"a") {
		t.Fatalf("unchanged row repainted while growing: %q", got)
	}
}

func TestUnit_CommitLiveTallerThanTerminalRendersTail(t *testing.T) {
	p, buf := newTestPainter(20, 3)
	got := commit(t, p, buf, frame.Frame{
		Live:   lines("a", "b", "c", "d", "e"),
		Cursor: frame.Cursor{Row: 4},
	})
	if strings.Contains(got, "a") || strings.Contains(got, "b") {
		t.Fatalf("rows above the terminal's height were painted: %q", got)
	}
	for _, want := range []string{"c", "d", "e"} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing row %q in %q", want, got)
		}
	}
	if n := strings.Count(got, seqClearLine); n != 3 {
		t.Fatalf("painted %d rows, want 3 (the terminal's height)", n)
	}
}

// TestUnit_ScrollbackLongLineEmittedRaw is acceptance test 1 of the
// copy/paste ruling made mechanical (blueprint section 1): a code line three
// times wider than the terminal reaches history as ONE contiguous run with
// exactly one trailing "\r\n". The terminal soft-wraps it, so it stays one
// logical line for native selection. Any "\r\n" inside that run would be a
// real line break in scrollback, and pasting the selected code would come
// back with phantom newlines in it — the defect this test exists to prevent.
func TestUnit_ScrollbackLongLineEmittedRaw(t *testing.T) {
	const width = 10
	cases := []struct {
		name string
		text string
	}{
		{"narrower than the terminal", "0123456"},
		// Exactly `width` cells is the auto-margin edge: the terminal defers
		// the wrap, and the "\r\n" that follows resolves it into exactly one
		// row of history with no blank row after it.
		{"exactly the terminal width", "0123456789"},
		{"three times the terminal width", strings.Repeat("0123456789", 3)},
		{"wide runes past the width", "日本語テストです"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p, buf := newTestPainter(width, 10)
			got := commit(t, p, buf, frame.Frame{
				Scrollback: []frame.Line{frame.Plain(c.text)},
				Live:       lines("live"),
				Cursor:     frame.Cursor{Hidden: true},
			})
			head := seqSyncBegin + seqCursorHide + "\r" + seqClearLine + c.text + "\r\n"
			if !strings.HasPrefix(got, head) {
				t.Fatalf("scrollback line was not printed raw\n got %q\nwant prefix %q", got, head)
			}
			if n := strings.Count(got, "\r\n"); n != 1 {
				t.Fatalf("history for one logical line contains %d line breaks, want exactly the trailing one: %q", n, got)
			}
			if n := strings.Count(got, c.text); n != 1 {
				t.Fatalf("scrollback line written %d times, want once: %q", n, got)
			}
			if body, _, _ := strings.Cut(visible(got), "\r\n"); strings.TrimPrefix(body, "\r") != c.text {
				t.Fatalf("history text = %q, want the line verbatim %q", body, c.text)
			}
			if !strings.HasSuffix(got, "\r"+seqClearLine+"live"+seqSyncEnd) {
				t.Fatalf("live region was not re-anchored below the printed line: %q", got)
			}
		})
	}
}

// The auto-margin edge, twice in a row: a history line of exactly `width`
// cells leaves the terminal with a pending wrap, and the next line's CR
// resolves it without consuming a row. Two full-width history lines therefore
// produce two rows of history — not four, and not one with a blank between.
func TestUnit_ScrollbackWidthExactLineFollowedByAnother(t *testing.T) {
	const width = 10
	p, buf := newTestPainter(width, 10)
	got := commit(t, p, buf, frame.Frame{
		Scrollback: lines("0123456789", "next line is longer than the terminal"),
		Live:       lines("live"),
		Cursor:     frame.Cursor{Hidden: true},
	})
	want := seqSyncBegin + seqCursorHide +
		"\r" + seqClearLine + "0123456789" + "\r\n" +
		"\r" + seqClearLine + "next line is longer than the terminal" + "\r\n" +
		"\r" + seqClearLine + "live" +
		seqSyncEnd
	if got != want {
		t.Fatalf("consecutive scrollback lines\n got %q\nwant %q", got, want)
	}
	if n := strings.Count(got, "\r\n"); n != 2 {
		t.Fatalf("two logical history lines produced %d line breaks, want one each", n)
	}
	if regexp.MustCompile(`\x1b\[[0-9]*[AB]`).MatchString(got) {
		t.Fatalf("history printing tried to predict the terminal's wrapping: %q", got)
	}
}

func TestUnit_CommitLiveRowTruncatedToWidth(t *testing.T) {
	p, buf := newTestPainter(8, 10)
	got := commit(t, p, buf, frame.Frame{
		Live:   lines("0123456789abc"),
		Cursor: frame.Cursor{Hidden: true},
	})
	if body := visible(got); body != "\r01234567" {
		t.Fatalf("live row not truncated to width: %q", body)
	}
}

func TestUnit_RenderLineIsRuneSafeAtTheWidthBoundary(t *testing.T) {
	p, _ := newTestPainter(5, 10)
	got := p.renderLine(frame.L(frame.S(frame.StyleNone, "日本語")), 5)
	if w := textwidth.Width(got); w > 5 {
		t.Fatalf("rendered %q is %d cells, want <= 5", got, w)
	}
	if got != "日本" {
		t.Fatalf("split a wide rune: %q", got)
	}
}

func TestUnit_CommitCursorPlacement(t *testing.T) {
	cases := []struct {
		name   string
		cursor frame.Cursor
		want   string
	}{
		{"top left", frame.Cursor{Row: 0, Col: 0}, cursorUp(2) + "\r" + seqCursorShow},
		{"middle", frame.Cursor{Row: 1, Col: 4}, cursorUp(1) + "\r" + cursorRight(4) + seqCursorShow},
		{"clamped past the region", frame.Cursor{Row: 9, Col: 99}, "\r" + cursorRight(19) + seqCursorShow},
		{"hidden", frame.Cursor{Hidden: true}, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p, buf := newTestPainter(20, 10)
			got := commit(t, p, buf, frame.Frame{Live: lines("a", "b", "c"), Cursor: c.cursor})
			tail := strings.TrimSuffix(got, seqSyncEnd)
			tail = tail[strings.LastIndex(tail, seqClearLine+"c")+len(seqClearLine+"c"):]
			if tail != c.want {
				t.Fatalf("cursor placement\n got %q\nwant %q", tail, c.want)
			}
			if c.cursor.Hidden && strings.Contains(got, seqCursorShow) {
				t.Fatalf("hidden cursor was shown: %q", got)
			}
		})
	}
}

// A live region taller than the terminal shows its tail, and the caret is
// addressed in the FRAME's coordinates — so it has to move up by however many
// rows the clamp dropped, or it lands on the wrong line entirely.
func TestUnit_CommitClampedLiveRemapsTheCursorRow(t *testing.T) {
	cases := []struct {
		name   string
		cursor frame.Cursor
		want   string
	}{
		// 5 rows clamped to the last 3 (c, d, e); painting ends on "e".
		{"caret on the last row", frame.Cursor{Row: 4, Col: 1}, "\r" + cursorRight(1) + seqCursorShow},
		{"caret one row up", frame.Cursor{Row: 3, Col: 1}, cursorUp(1) + "\r" + cursorRight(1) + seqCursorShow},
		{"caret on the first visible row", frame.Cursor{Row: 2}, cursorUp(2) + "\r" + seqCursorShow},
		// A caret in a row the clamp scrolled off pins to the top of what is
		// visible rather than to the bottom of the region.
		{"caret in a dropped row", frame.Cursor{Row: 0}, cursorUp(2) + "\r" + seqCursorShow},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p, buf := newTestPainter(20, 3)
			got := commit(t, p, buf, frame.Frame{
				Live:   lines("a", "b", "c", "d", "e"),
				Cursor: c.cursor,
			})
			tail := strings.TrimSuffix(got, seqSyncEnd)
			tail = tail[strings.LastIndex(tail, seqClearLine+"e")+len(seqClearLine+"e"):]
			if tail != c.want {
				t.Fatalf("cursor placement under a clamped region\n got %q\nwant %q", tail, c.want)
			}
		})
	}
}

// An empty live region has no row to address, but the cursor was hidden on
// the way into the frame: leaving it hidden hands the user an invisible caret
// for as long as the region stays empty.
func TestUnit_CommitEmptyLiveRegionStillShowsTheCursor(t *testing.T) {
	p, buf := newTestPainter(20, 10)
	commit(t, p, buf, frame.Frame{Live: lines("a"), Cursor: frame.Cursor{Row: 0}})
	got := commit(t, p, buf, frame.Frame{Cursor: frame.Cursor{Row: 0}})
	if !strings.Contains(got, seqCursorShow) {
		t.Fatalf("empty region left the cursor hidden: %q", got)
	}
	if !strings.HasSuffix(got, seqCursorShow+seqSyncEnd) {
		t.Fatalf("cursor shown before the frame closed: %q", got)
	}

	p2, buf2 := newTestPainter(20, 10)
	commit(t, p2, buf2, frame.Frame{Live: lines("a"), Cursor: frame.Cursor{Row: 0}})
	hidden := commit(t, p2, buf2, frame.Frame{Cursor: frame.Cursor{Hidden: true}})
	if strings.Contains(hidden, seqCursorShow) {
		t.Fatalf("hidden cursor was shown on an empty region: %q", hidden)
	}
}

func TestUnit_CommitIsAlwaysSynchronizedAndHidesTheCursor(t *testing.T) {
	p, buf := newTestPainter(20, 10)
	frames := []frame.Frame{
		{Live: lines("a"), Cursor: frame.Cursor{}},
		{Scrollback: lines("h"), Live: lines("a"), Cursor: frame.Cursor{}},
		{Live: lines("a", "b"), Cursor: frame.Cursor{Row: 1}},
		{Live: lines("a"), Cursor: frame.Cursor{Hidden: true}},
	}
	for i, f := range frames {
		got := commit(t, p, buf, f)
		if got == "" {
			t.Fatalf("frame %d wrote nothing", i)
		}
		if !strings.HasPrefix(got, seqSyncBegin+seqCursorHide) {
			t.Fatalf("frame %d does not open synchronized+hidden: %q", i, got)
		}
		if !strings.HasSuffix(got, seqSyncEnd) {
			t.Fatalf("frame %d does not close synchronized output: %q", i, got)
		}
	}
}

func TestUnit_CommitNeverClearsTheScreenOrJumpsHome(t *testing.T) {
	p, buf := newTestPainter(20, 4)
	// The hostile line is the same forbidden set arriving as span TEXT: if
	// spans were trusted, every sequence below would reach the terminal.
	hostile := frame.L(
		frame.S(frame.StyleNone, "\x1b[2J"),
		frame.S(frame.StyleNone, "\x1b[H\x1b[?1049h"),
		frame.S(frame.StyleNone, "\x1b[?1000h\x1b[?1006h"),
	)
	var all strings.Builder
	all.WriteString(commit(t, p, buf, frame.Frame{Live: lines("a", "b", "c"), Cursor: frame.Cursor{Row: 2}}))
	all.WriteString(commit(t, p, buf, frame.Frame{Scrollback: lines("h"), Live: lines("a"), Cursor: frame.Cursor{}}))
	p.resize(10, 3)
	all.WriteString(commit(t, p, buf, frame.Frame{Live: lines("a", "b"), Cursor: frame.Cursor{Row: 1}}))
	all.WriteString(commit(t, p, buf, frame.Frame{
		Scrollback: []frame.Line{hostile},
		Live:       []frame.Line{hostile},
		Cursor:     frame.Cursor{Row: 0},
	}))
	forbidden := []string{"\x1b[2J", "\x1b[J", "\x1b[H", "\x1b[?1049h", "\x1b[?1000h", "\x1b[?1006h"}
	for _, seq := range forbidden {
		if strings.Contains(all.String(), seq) {
			t.Fatalf("engine emitted forbidden sequence %q", seq)
		}
	}
	if regexp.MustCompile(`\x1b\[[0-9;]*H`).MatchString(all.String()) {
		t.Fatalf("engine used absolute cursor addressing: %q", all.String())
	}
}

// frame.Span is defined as printable cells, and the comp layer sanitizes text
// on the way in. term does not trust that: it is the last place a byte can
// become terminal behaviour, so it strips what a span may not carry rather
// than assuming nothing does. A span that slipped through unsanitized must be
// unable to clear the screen, ring the bell, or break the one-line-per-row
// geometry every cursor offset depends on.
func TestUnit_SpanTextIsStrippedOfControlBytes(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"escape sequence", "\x1b[2Jgone", "[2Jgone"},
		{"bell", "ding\a", "ding"},
		{"tab", "a\tb", "ab"},
		{"newline", "a\nb", "ab"},
		{"carriage return", "a\rb", "ab"},
		{"del", "a\x7fb", "ab"},
		{"c1 control rune", "a\u009bb", "ab"},
		{"c1 range low end", "a\u0080b", "ab"},
		{"c1 range high end", "a\u009fb", "ab"},
		{"raw c1 byte", "a\x9bb", "ab"},
		{"invalid utf8", "a\xffb", "ab"},
		{"nul", "a\x00b", "ab"},
		{"plain text is untouched", "hello", "hello"},
		{"wide runes are untouched", "日本語 é👍", "日本語 é👍"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p, buf := newTestPainter(80, 10)
			line := frame.L(frame.S(frame.StyleNone, c.in))
			if got := p.renderLine(line, 80); got != c.want {
				t.Fatalf("renderLine(%q) = %q, want %q", c.in, got, c.want)
			}
			if got := p.renderRaw(line); got != c.want {
				t.Fatalf("renderRaw(%q) = %q, want %q", c.in, got, c.want)
			}
			// And end to end, where a leaked ESC would be a live sequence.
			got := commit(t, p, buf, frame.Frame{
				Scrollback: []frame.Line{line},
				Live:       []frame.Line{line},
				Cursor:     frame.Cursor{Hidden: true},
			})
			body := strings.ReplaceAll(visible(got), "\r\n", "")
			if body != "\r"+c.want+"\r"+c.want {
				t.Fatalf("commit rendered %q, want the stripped text twice: %q", body, c.want)
			}
		})
	}
}

// A span whose entire content is control bytes renders to nothing, and the
// row it lives on still occupies exactly one physical row.
func TestUnit_SpanOfPureControlBytesRendersEmpty(t *testing.T) {
	p, buf := newTestPainter(20, 10)
	got := commit(t, p, buf, frame.Frame{
		Live:   []frame.Line{frame.L(frame.S(frame.StyleNone, "\x1b\a\x00\x7f\x9b"))},
		Cursor: frame.Cursor{Hidden: true},
	})
	want := seqSyncBegin + seqCursorHide + "\r" + seqClearLine + seqSyncEnd
	if got != want {
		t.Fatalf("control-only span\n got %q\nwant %q", got, want)
	}
}

func TestUnit_ResizeForcesAFullRepaint(t *testing.T) {
	p, buf := newTestPainter(20, 10)
	live := lines("one", "two")
	commit(t, p, buf, frame.Frame{Live: live, Cursor: frame.Cursor{Row: 0}})
	p.resize(30, 12)
	got := commit(t, p, buf, frame.Frame{Live: live, Cursor: frame.Cursor{Row: 0}})
	if n := strings.Count(got, seqClearLine); n != 2 {
		t.Fatalf("resize repainted %d rows, want both", n)
	}
	if !strings.Contains(got, "one") || !strings.Contains(got, "two") {
		t.Fatalf("resize did not repaint the whole region: %q", got)
	}
}

// A resize reflows the terminal: the rows the painter measured at the old
// width have been rewrapped, so it can no longer say where they are. It
// therefore disowns them exactly as reset() does — paints fresh in place and
// reclaims nothing — rather than moving up over rows that are not where it
// left them and interleaving two frames.
func TestUnit_ResizeDisownsTheRegion(t *testing.T) {
	p, buf := newTestPainter(20, 10)
	commit(t, p, buf, frame.Frame{Live: lines("a", "b", "c"), Cursor: frame.Cursor{Row: 2}})
	p.resize(30, 12)
	got := commit(t, p, buf, frame.Frame{Live: lines("x"), Cursor: frame.Cursor{Row: 0}})

	if regexp.MustCompile(`\x1b\[[0-9]*A`).MatchString(got) {
		t.Fatalf("commit after resize moved up over rows it can no longer locate: %q", got)
	}
	if strings.Contains(got, cursorDown(1)) {
		t.Fatalf("commit after resize cleared rows it no longer owns: %q", got)
	}
	want := seqSyncBegin + seqCursorHide + "\r" + seqClearLine + "x" + "\r" + seqCursorShow + seqSyncEnd
	if got != want {
		t.Fatalf("commit after resize\n got %q\nwant %q", got, want)
	}
	if p.prevRows != 1 {
		t.Fatalf("region owns %d rows after the resize commit, want just the one it painted", p.prevRows)
	}
}

// commit publishes its bookkeeping only after the write lands. Publishing it
// first describes a frame the screen may not be showing: the identical retry
// then diffs to nothing and the half-drawn frame stays on screen forever.
func TestUnit_CommitWriteFailureInvalidatesAndRepaints(t *testing.T) {
	w := &flakyWriter{err: errors.New("terminal went away")}
	p := &painter{out: w, styles: plainStyles{}, width: 20, height: 10}
	if err := p.commit(frame.Frame{Live: lines("one", "two"), Cursor: frame.Cursor{Row: 0}}); err != nil {
		t.Fatalf("first commit: %v", err)
	}
	landed := append([]string(nil), p.prev...)

	next := frame.Frame{Live: lines("one", "CHANGED"), Cursor: frame.Cursor{Row: 1}}
	w.failOn, w.writes = 1, 0
	w.buf.Reset()
	if err := p.commit(next); err == nil {
		t.Fatal("commit swallowed a failed write")
	}
	if !p.invalid {
		t.Fatal("a failed write left the diff cache valid")
	}
	if !sameRows(p.prev, landed) {
		t.Fatalf("bookkeeping published a frame that never reached the screen: %q", p.prev)
	}
	if !strings.HasSuffix(w.buf.String(), seqSyncEnd+seqCursorShow) {
		t.Fatalf("a failed frame left the terminal synchronized and blind: %q", w.buf.String())
	}
	if w.writes != 2 {
		t.Fatalf("failed commit made %d writes, want the frame plus one recovery attempt", w.writes)
	}

	// The retry is the SAME frame: it must still repaint everything, because
	// what the failed write left on screen is unknowable.
	w.failOn, w.writes = 0, 0
	w.buf.Reset()
	if err := p.commit(next); err != nil {
		t.Fatalf("retry: %v", err)
	}
	got := w.buf.String()
	if n := strings.Count(got, seqClearLine); n != 2 {
		t.Fatalf("retry repainted %d rows, want the whole region: %q", n, got)
	}
	if !strings.Contains(got, "one") || !strings.Contains(got, "CHANGED") {
		t.Fatalf("retry did not repaint every row: %q", got)
	}
}

// One frame, one Write. The single write is what makes a frame's failure
// semantics decidable at all (see the failure test above) and what keeps a
// synchronized-output bracket from being split across syscalls.
func TestUnit_CommitPerformsExactlyOneWrite(t *testing.T) {
	w := &flakyWriter{}
	p := &painter{out: w, styles: plainStyles{}, width: 20, height: 4}
	frames := []struct {
		name string
		f    frame.Frame
	}{
		{"first paint", frame.Frame{Live: lines("a", "b"), Cursor: frame.Cursor{Row: 0}}},
		{"one row changed", frame.Frame{Live: lines("a", "B"), Cursor: frame.Cursor{Row: 0}}},
		{"cursor only", frame.Frame{Live: lines("a", "B"), Cursor: frame.Cursor{Row: 1, Col: 1}}},
		{"scrollback", frame.Frame{Scrollback: lines("h1", "h2"), Live: lines("a", "B"), Cursor: frame.Cursor{Row: 1, Col: 1}}},
		{"region grows", frame.Frame{Live: lines("a", "B", "c"), Cursor: frame.Cursor{Row: 2}}},
		{"region empties", frame.Frame{Cursor: frame.Cursor{Hidden: true}}},
		{"region returns", frame.Frame{Live: lines("z"), Cursor: frame.Cursor{Row: 0}}},
	}
	for _, c := range frames {
		before := w.writes
		if err := p.commit(c.f); err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if n := w.writes - before; n != 1 {
			t.Fatalf("%s made %d writes, want exactly 1", c.name, n)
		}
	}
	before := w.writes
	if err := p.commit(frames[len(frames)-1].f); err != nil { // identical frame
		t.Fatalf("idle recommit: %v", err)
	}
	if n := w.writes - before; n != 0 {
		t.Fatalf("a frame that changes nothing made %d writes, want none", n)
	}
}

func TestUnit_ResetStartsAFreshRegionInPlace(t *testing.T) {
	p, buf := newTestPainter(20, 10)
	commit(t, p, buf, frame.Frame{Live: lines("a", "b", "c"), Cursor: frame.Cursor{Row: 2}})
	p.reset()
	got := commit(t, p, buf, frame.Frame{Live: lines("a"), Cursor: frame.Cursor{Row: 0}})
	if strings.Contains(got, "\x1b[2A") || strings.Contains(got, "\x1b[1A") {
		t.Fatalf("commit after reset reclaimed rows it no longer owns: %q", got)
	}
	if strings.Contains(got, cursorDown(1)) {
		t.Fatalf("commit after reset cleared rows it no longer owns: %q", got)
	}
	want := seqSyncBegin + seqCursorHide + "\r" + seqClearLine + "a" + "\r" + seqCursorShow + seqSyncEnd
	if got != want {
		t.Fatalf("commit after reset\n got %q\nwant %q", got, want)
	}
}

func TestUnit_CommitEmptyLiveRegionClearsAndRecovers(t *testing.T) {
	p, buf := newTestPainter(20, 10)
	commit(t, p, buf, frame.Frame{Live: lines("a", "b"), Cursor: frame.Cursor{Row: 0}})
	got := commit(t, p, buf, frame.Frame{Cursor: frame.Cursor{Hidden: true}})
	if !strings.Contains(got, cursorDown(1)+"\r"+seqClearLine) {
		t.Fatalf("empty live region left the old rows on screen: %q", got)
	}
	back := commit(t, p, buf, frame.Frame{Live: lines("x"), Cursor: frame.Cursor{Row: 0}})
	if !strings.Contains(back, seqClearLine+"x") {
		t.Fatalf("region did not come back: %q", back)
	}
}

func TestUnit_RenderLineResolvesEverySpan(t *testing.T) {
	styles := &tagStyles{}
	var buf bytes.Buffer
	p := &painter{out: &buf, styles: styles, width: 40, height: 10}
	line := frame.L(
		frame.S(frame.StyleUser, "you"),
		frame.S(frame.StyleNone, ": "),
		frame.S(frame.StyleCode, "beam"),
	)
	if err := p.commit(frame.Frame{Live: []frame.Line{line}, Cursor: frame.Cursor{Hidden: true}}); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if want := "<user>you</user>: <code>beam</code>"; !strings.Contains(buf.String(), want) {
		t.Fatalf("styled render\n got %q\nwant it to contain %q", buf.String(), want)
	}
	wantCalls := []frame.StyleID{frame.StyleUser, frame.StyleNone, frame.StyleCode}
	if len(styles.calls) != len(wantCalls) {
		t.Fatalf("resolver called %v, want one call per span %v", styles.calls, wantCalls)
	}
	for i, id := range wantCalls {
		if styles.calls[i] != id {
			t.Fatalf("resolver call %d was %q, want %q", i, styles.calls[i], id)
		}
	}
}

// Scrollback goes through the resolver span by span like any other line; the
// only difference from the live region is that width does not clamp it.
func TestUnit_ScrollbackResolvesSpansWithoutTruncating(t *testing.T) {
	styles := &tagStyles{}
	var buf bytes.Buffer
	p := &painter{out: &buf, styles: styles, width: 4, height: 10}
	line := frame.L(frame.S(frame.StyleUser, "aaaa"), frame.S(frame.StyleCode, "bbbb"))
	if err := p.commit(frame.Frame{
		Scrollback: []frame.Line{line},
		Live:       lines("x"),
		Cursor:     frame.Cursor{Hidden: true},
	}); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if want := "<user>aaaa</user><code>bbbb</code>\r\n"; !strings.Contains(buf.String(), want) {
		t.Fatalf("scrollback render\n got %q\nwant it to contain %q", buf.String(), want)
	}
	// The live row is rendered before history, then one call per history span.
	wantCalls := []frame.StyleID{frame.StyleNone, frame.StyleUser, frame.StyleCode}
	if len(styles.calls) != len(wantCalls) {
		t.Fatalf("resolver called %v, want one call per span %v", styles.calls, wantCalls)
	}
	for i, id := range wantCalls {
		if styles.calls[i] != id {
			t.Fatalf("resolver call %d was %q, want %q", i, styles.calls[i], id)
		}
	}
}

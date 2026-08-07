package term

import (
	"strings"
	"testing"

	"github.com/contenox/contenox/internal/surfaces/beamtui/frame"
)

// The fixture is the shape every real beam frame has — committed history above,
// then transcript tail / blank / composer hint / composer input / status bar,
// with the caret MID-region in the composer input. It is deliberately ASCII:
// the screen model counts runes, so a wide rune would make the model and
// textwidth.Width disagree about something the painter is not on trial for.
const (
	histA  = "> summarize the design doc"
	histB  = "assistant: the design doc argues for a portable envelope format"
	tail   = "assistant: ...and that is why the origin anchor matters here"
	hint   = "| type / for commands - ! for shell - @ to attach"
	typing = "> and then what"
	status = "contenox  beam-629a9e4f-1  761/131072 (0%)  gemini-2.5-flash - vertex-google"

	// Substrings short enough to survive truncation at every width tested, so
	// counting them counts residue rather than accidents of truncation.
	hintMark   = "type / for commands"
	statusMark = "beam-629a9e4f"
)

func realFrame(withHistory bool) frame.Frame {
	f := frame.Frame{
		Live:   lines(tail, "", hint, typing, status),
		Cursor: frame.Cursor{Row: 3, Col: len(typing)},
	}
	if withHistory {
		f.Scrollback = lines(histA, histB)
	}
	return f
}

// paintedRegion drives the painter through a whole gesture: an initial commit
// at width, the terminal reflowing through every width in drag, then the single
// resize the engine's debounce collapses that drag into, then the repaint.
func paintedRegion(t *testing.T, width int, drag []int, final int) *vt {
	t.Helper()
	scr := newScreen(width)
	p := &painter{out: scr, styles: plainStyles{}, width: width, height: 20}
	if err := p.commit(realFrame(true)); err != nil {
		t.Fatalf("initial commit: %v", err)
	}
	for _, w := range drag {
		scr.resize(w)
	}
	p.resize(final, 20)
	if err := p.commit(realFrame(false)); err != nil {
		t.Fatalf("repaint after resize: %v", err)
	}
	return scr
}

// assertHistoryIntact pins the invariant that must never break: lines already
// committed to the terminal's own scrollback are the user's, and no resize may
// rewrite, erase or duplicate them.
func assertHistoryIntact(t *testing.T, scr *vt) {
	t.Helper()
	got := scr.logical()
	want := []string{histA, histB}
	if len(got) < len(want) {
		t.Fatalf("screen lost committed history entirely:\n%s", scr.text())
	}
	for i, line := range want {
		if got[i] != line {
			t.Fatalf("committed history line %d = %q, want %q\nscreen:\n%s", i, got[i], line, scr.text())
		}
	}
	for _, line := range want {
		if n := scr.count(line); n != 1 {
			t.Fatalf("committed history line %q appears %d times, want once\nscreen:\n%s", line, n, scr.text())
		}
	}
}

// TestUnit_ScreenNarrowThenWidenLeavesChromeOnce is the maintainer's repro:
// drag the terminal narrow and back out again. Every intermediate width used to
// leave its own truncated copy of the composer hint and the status bar in
// scrollback, which is why the fragments came out truncated at different
// widths. Debouncing collapses the drag to one resize, and by then the
// terminal has unwrapped back to the geometry the painter's counts describe, so
// the erase lands exactly on the origin.
func TestUnit_ScreenNarrowThenWidenLeavesChromeOnce(t *testing.T) {
	scr := paintedRegion(t, 72, []int{60, 47, 33, 22, 15, 22, 33, 47, 60, 72}, 72)

	for _, mark := range []string{hintMark, statusMark} {
		if n := scr.count(mark); n != 1 {
			t.Fatalf("%q appears %d times after a narrow-and-widen drag, want once\nscreen:\n%s", mark, n, scr.text())
		}
	}
	assertHistoryIntact(t, scr)
}

// TestUnit_ScreenDragEndingWiderLeavesChromeOnce covers a drag that stops wider
// than it started. Widening can never strand rows above the origin: each live
// row is its own logical line no wider than the width it was painted at, so
// there is nothing for the terminal to rejoin and the caret's distance to the
// origin is exactly p.row.
func TestUnit_ScreenDragEndingWiderLeavesChromeOnce(t *testing.T) {
	scr := paintedRegion(t, 72, []int{64, 50, 38, 26, 40, 66, 84, 96}, 96)

	for _, mark := range []string{hintMark, statusMark} {
		if n := scr.count(mark); n != 1 {
			t.Fatalf("%q appears %d times after a drag ending wider, want once\nscreen:\n%s", mark, n, scr.text())
		}
	}
	assertHistoryIntact(t, scr)
}

// TestUnit_ScreenDragEndingNarrowerSweepsBelowTheOrigin covers the case the
// origin-anchored ED0 exists for: the drag comes to rest NARROWER than the
// region was painted at, so the terminal has rewrapped rows the painter counted
// as one. Everything from the erase's landing row down is swept regardless of
// how many rows the rewrap produced, which is what stops the status bar — the
// row that always fills the width and so always rewraps — from being orphaned
// below the repaint.
//
// The rows the erase cannot reach are those the rewrap pushed ABOVE the caret;
// here that is the transcript tail, which is live-region content the repaint
// reproduces. See painter.eraseRegion for why no attempt is made to walk up
// further.
func TestUnit_ScreenDragEndingNarrowerSweepsBelowTheOrigin(t *testing.T) {
	scr := paintedRegion(t, 72, []int{60, 48, 36, 30}, 30)

	if n := scr.count(statusMark); n != 1 {
		t.Fatalf("%q appears %d times after a drag ending narrower, want once\nscreen:\n%s", statusMark, n, scr.text())
	}
	if n := scr.count(hintMark); n != 1 {
		t.Fatalf("%q appears %d times after a drag ending narrower, want once\nscreen:\n%s", hintMark, n, scr.text())
	}
	assertHistoryIntact(t, scr)

	// The region the repaint produced is the tail of the screen, in order and
	// at the new width.
	got := strings.Split(scr.text(), "\n")
	// The screen reports rows without their trailing blanks, as a terminal does.
	want := []string{strings.TrimRight(hint[:30], " "), typing, status[:30]}
	if len(got) < len(want) {
		t.Fatalf("screen is shorter than the region it should end with:\n%s", scr.text())
	}
	if diff := got[len(got)-len(want):]; !equalLines(diff, want) {
		t.Fatalf("screen ends with %q, want the repainted region %q\nscreen:\n%s", diff, want, scr.text())
	}
}

// TestUnit_ScreenRepeatedDragsDoNotAccumulate pins that residue does not
// compound: whatever one settled narrowing strands, the next erase's sweep from
// the origin takes back, so chrome cannot pile up over a session's worth of
// window resizing.
func TestUnit_ScreenRepeatedDragsDoNotAccumulate(t *testing.T) {
	scr := newScreen(72)
	p := &painter{out: scr, styles: plainStyles{}, width: 72, height: 20}
	if err := p.commit(realFrame(true)); err != nil {
		t.Fatalf("initial commit: %v", err)
	}
	for _, settled := range []int{40, 72, 34, 90, 28, 72} {
		for _, w := range []int{settled + 9, settled + 4, settled} {
			scr.resize(w)
		}
		p.resize(settled, 20)
		if err := p.commit(realFrame(false)); err != nil {
			t.Fatalf("repaint at width %d: %v", settled, err)
		}
	}
	for _, mark := range []string{hintMark, statusMark} {
		if n := scr.count(mark); n != 1 {
			t.Fatalf("%q appears %d times after six drags, want once\nscreen:\n%s", mark, n, scr.text())
		}
	}
	assertHistoryIntact(t, scr)
}

// TestUnit_ScreenModelReflowsLikeATerminal guards the model itself: a test
// harness that does not rewrap cannot see the defect these tests exist for.
func TestUnit_ScreenModelReflowsLikeATerminal(t *testing.T) {
	scr := newScreen(10)
	if _, err := scr.Write([]byte("abcdefghijklmnopqrst\r\nnext")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if got, want := scr.text(), "abcdefghij\nklmnopqrst\nnext"; got != want {
		t.Fatalf("auto-wrap produced %q, want %q", got, want)
	}
	scr.resize(5)
	if got, want := scr.text(), "abcde\nfghij\nklmno\npqrst\nnext"; got != want {
		t.Fatalf("narrowing produced %q, want a rewrap to %q", got, want)
	}
	scr.resize(20)
	if got, want := scr.text(), "abcdefghijklmnopqrst\nnext"; got != want {
		t.Fatalf("widening produced %q, want the logical lines rejoined to %q", got, want)
	}
}

func equalLines(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// Package testkit is beam's shared test infrastructure: the reference
// encoding that turns frame data into reviewable golden files, the golden
// compare/update helper, and (as the suites grow) the fixture corpus,
// FakeEngineBridge, and liveness frame-diff harness.
//
// Goldens are Frame data through Encode — readable style tags, no escape
// codes — so a diff is reviewable by a human AND catches style regressions
// (decision D54 in the beam blueprint).
package testkit

import (
	"fmt"
	"strings"

	"github.com/contenox/beam/internal/surfaces/beamtui/frame"
)

// EncodeLines renders lines in the reference golden encoding: spans under a
// style are wrapped `[style]text[/]`; unstyled spans are bare text. One
// terminal row per output line.
//
// It also ENFORCES [frame.Span]'s text contract, panicking when a span
// carries a C0 control or DEL (see checkSpan). Every component's golden test
// runs through here, so the golden layer doubles as the backstop for the one
// span invariant a reviewer cannot see in a diff: a tab or an escape encodes
// into a golden file as something that looks fine and draws as something
// else. Producing such a span is a component bug, not a test failure to
// re-baseline with -update.
func EncodeLines(lines []frame.Line) string {
	var b strings.Builder
	for _, l := range lines {
		checkLine(l)
		encodeLine(&b, l)
		b.WriteByte('\n')
	}
	return b.String()
}

// checkLine panics on the first span that breaks the literal-cells contract.
//
// It panics rather than taking a *testing.T because EncodeLines is called
// from render helpers all over the suite (and from EncodeFrame), and threading
// a T through every one of them would buy nothing: the panic names the line,
// the span and the offending rune, which is exactly what the fix needs.
func checkLine(l frame.Line) {
	for i, s := range l {
		for _, r := range s.Text {
			if r >= 0x20 && r != 0x7f {
				continue
			}
			what := "control rune"
			switch r {
			case '\n':
				what = "newline (frame.Line holds ONE terminal row; wrap in the component)"
			case '\t':
				what = "tab (expand it with sanitize.ExpandTabs before it becomes a span)"
			case 0x1b:
				what = "escape (run untrusted text through the sanitize package at ingest)"
			}
			panic(fmt.Sprintf(
				"testkit: span %d of line %q carries %U — %s. frame.Span text is drawn as literal cells and must hold none.",
				i, l.Text(), r, what))
		}
	}
}

// EncodeFrame renders a full frame: the scrollback section, the live
// section, and the cursor position, each explicitly labeled so goldens for
// commit-shaped tests stay unambiguous.
func EncodeFrame(f frame.Frame) string {
	var b strings.Builder
	b.WriteString("── scrollback ──\n")
	b.WriteString(EncodeLines(f.Scrollback))
	if f.Cursor.Hidden {
		b.WriteString("── live (cursor hidden) ──\n")
	} else {
		fmt.Fprintf(&b, "── live (cursor %d,%d) ──\n", f.Cursor.Row, f.Cursor.Col)
	}
	b.WriteString(EncodeLines(f.Live))
	return b.String()
}

func encodeLine(b *strings.Builder, l frame.Line) {
	for _, s := range l {
		if s.Style == frame.StyleNone {
			b.WriteString(s.Text)
			continue
		}
		b.WriteByte('[')
		b.WriteString(string(s.Style))
		b.WriteByte(']')
		b.WriteString(s.Text)
		b.WriteString("[/]")
	}
}

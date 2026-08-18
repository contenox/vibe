// Package testkit is beam's shared test infrastructure: the reference
// encoding that turns frame data into reviewable golden files, the golden
// compare/update helper, the fixture corpus, FakeEngineBridge, and the
// liveness frame-diff harness. Goldens use readable style tags instead of
// escape codes, so a diff is human-reviewable and still catches style
// regressions.
package testkit

import (
	"fmt"
	"strings"

	"github.com/contenox/contenox/internal/surfaces/beam/frame"
)

// EncodeLines renders lines in the reference golden encoding: a styled span
// is wrapped `[style]text[/]`, an unstyled span is bare text, one output
// line per terminal row.
//
// It enforces [frame.Span]'s text contract, panicking when a span carries a
// C0 control or DEL: such a span would encode unremarkably but draw as
// something else, so producing one is a component bug, not a reason to
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
// It takes no *testing.T since EncodeLines is called from render helpers
// throughout the suite; the panic names the line, span, and offending rune.
func checkLine(l frame.Line) {
	for i, s := range l {
		for _, r := range s.Text {
			if r >= 0x20 && r != 0x7f {
				continue
			}
			what := "control rune"
			switch r {
			case '\n':
				what = "newline (frame.Line holds one terminal row; wrap in the component)"
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

// EncodeFrame renders a full frame: scrollback, live section, and cursor
// position, each explicitly labeled so goldens stay unambiguous.
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

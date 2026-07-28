// Package brand renders beam's identity device: the fresh-session welcome
// header and the persistent status-bar identity segment, both built around
// the vertical brand-mint beam-bar `▌` and, once per session, the logo-mark as
// block art. Everything is a pure function of (state, width) → []frame.Line
// — no terminal reads, no SGR — with ASCII fallback via the caller's Caps.
package brand

import (
	"strings"

	"github.com/contenox/contenox/internal/surfaces/beamtui/frame"
	"github.com/contenox/contenox/internal/surfaces/beamtui/textwidth"
)

// CompactWidth is the narrowest width that still fits the logo-mark header;
// below it Welcome drops the art and prints two rows instead.
const CompactWidth = 66

// wordmark and tagline are fixed brand copy: lowercase "contenox", em-dash
// in unicode, plain hyphen in ASCII — do not paraphrase either string.
const (
	wordmark       = "contenox beam"
	taglineUnicode = " — open coding harness"
	taglineASCII   = " - open coding harness"
)

// ASCIIGutter is the beam-bar a Mono terminal sees, exported so testkit's
// glyph-parity test can check it against style's GlyphSet without this
// package importing style.
const ASCIIGutter = "|"

const (
	gutterUnicode = "▌"
	gutterASCII   = ASCIIGutter

	// metaSep joins model and provider on the session line.
	metaSepUnicode = " · "
	metaSepASCII   = " - "

	ellipsisUnicode = "…"
	ellipsisASCII   = "..."

	// gutterGap separates the beam-bar from the content it marks;
	// wordmarkGap separates the art column from the wordmark.
	gutterGap   = "  "
	wordmarkGap = 2
)

// artSpan is one styled run inside an art row. Rows are span lists because
// the mark's blades interleave within a row.
type artSpan struct {
	style frame.StyleID
	text  string
}

// artUnicode is the logo-mark rasterized from website/public/logo-mark.svg:
// three blades — quarter-arc outer edge, straight inner edge, mitered ends —
// pinwheel-arranged around a square core, opening right. Rendered via
// half-blocks (one text row = two pixel rows) in the favicon's ramp,
// lightest at top; keep any future change faithful to the source mark.
var artUnicode = [5][]artSpan{
	{{frame.StyleNone, "    "}, {frame.StyleBrandRamp1, "▄▄███"}},
	{{frame.StyleNone, " "}, {frame.StyleBrandRamp2, "▄▄"}, {frame.StyleNone, " "}, {frame.StyleBrandRamp1, "▀▀▀▀▀"}},
	{{frame.StyleBrandRamp2, "▄██"}},
	{{frame.StyleBrandRamp2, "███"}, {frame.StyleNone, " "}, {frame.StyleBrandRamp3, "▄▄▄▄▄"}},
	{{frame.StyleNone, "    "}, {frame.StyleBrandRamp3, "▀▀███"}},
}

// artASCII suggests the same swirl in characters a legacy console can draw:
// diagonals stand in for the arc, gaps stay. The mark is symmetric about its
// middle row, matching the unicode art's mirrored top/bottom edges, so ASCII
// reads as the same open C rather than a lopsided hook; tildes and
// underscores approximate ▀/▄ at the two heights ASCII offers.
var artASCII = [5][]artSpan{
	{{frame.StyleNone, "    "}, {frame.StyleBrandRamp1, ",==="}},
	{{frame.StyleNone, "   "}, {frame.StyleBrandRamp2, "/"}, {frame.StyleNone, " "}, {frame.StyleBrandRamp1, "~~~"}},
	{{frame.StyleNone, "  "}, {frame.StyleBrandRamp2, "|"}},
	{{frame.StyleNone, "   "}, {frame.StyleBrandRamp2, "\\"}, {frame.StyleNone, " "}, {frame.StyleBrandRamp3, "___"}},
	{{frame.StyleNone, "    "}, {frame.StyleBrandRamp3, "`==="}},
}

// wordmarkRow is the art row the wordmark hangs beside, the mark's vertical middle.
const wordmarkRow = 2

// hint is one key and what it opens. Keys print unstyled so they read as
// literal keystrokes; the label carries the muted style.
type hint struct{ key, label string }

// fullHints is the welcome header's hint line: the affordances a first-run user cannot discover by typing.
var fullHints = []hint{
	{"/", "commands"},
	{"@", "files"},
	{"!", "shell"},
	{"Ctrl+X Ctrl+E", "editor"},
	{"?", "keys"},
}

// compactHints is the same set at narrow widths, abbreviated so every affordance survives.
const compactHints = "/ cmds  @ files  ! shell  ^X^E editor  ? keys"

// identity is the status bar's wordmark: the product name, not the surface's.
const identity = "contenox"

// Info is the session context the welcome header may show. The zero value
// renders the pure brand moment — no model, no provider, no session.
//
// ASCII selects the character fallback and must be true exactly when the
// caller's caps profile is Mono; this package never probes for it itself.
type Info struct {
	ASCII    bool
	Model    string
	Provider string
	Session  string
}

// Welcome renders the fresh-session header for width, printed once into
// scrollback (never the live region) so it survives resize as literal
// history.
//
// At width >= CompactWidth the layout is the logo-mark art beside the
// wordmark, an optional session line, and one hint line; below that it
// collapses to wordmark + abbreviated hints. The last line is always an
// empty separator, so the caller can append transcript directly without
// spacing of its own. No returned line is ever wider than width.
func Welcome(width int, info Info) []frame.Line {
	var lines []frame.Line
	if width < CompactWidth {
		lines = compact(info)
	} else {
		lines = full(info)
	}
	for i, l := range lines {
		lines[i] = clamp(l, width, info.ASCII)
	}
	// The separator is unconditional: it is what makes the header safe to
	// print directly ahead of transcript content.
	return append(lines, frame.Plain(""))
}

// full is the logo-mark layout. Row 4 is the bare gutter — the beam-bar
// runs unbroken past the art so the device reads as one stroke rather than
// three decorations.
func full(info Info) []frame.Line {
	g := gutter(info.ASCII)
	art := artFor(info.ASCII)
	artW := 0
	for _, row := range art {
		w := 0
		for _, s := range row {
			w += textwidth.Width(s.text)
		}
		if w > artW {
			artW = w
		}
	}

	var lines []frame.Line
	for i, row := range art {
		spans := []frame.Span{
			frame.S(frame.StyleBrand, g),
			frame.S(frame.StyleNone, gutterGap),
		}
		rowW := 0
		for _, s := range row {
			spans = append(spans, frame.S(s.style, s.text))
			rowW += textwidth.Width(s.text)
		}
		if i == wordmarkRow {
			// Hang the wordmark off a fixed art column so it lands in
			// the same place whichever variant drew the mark.
			spans = append(spans,
				frame.S(frame.StyleNone, strings.Repeat(" ", artW-rowW+wordmarkGap)),
				frame.S(frame.StyleStrong, wordmark),
				frame.S(frame.StyleMuted, tagline(info.ASCII)),
			)
		}
		lines = append(lines, frame.L(spans...))
	}
	lines = append(lines, frame.L(frame.S(frame.StyleBrand, g)))

	if info.Model != "" {
		lines = append(lines, frame.L(
			frame.S(frame.StyleBrand, g),
			frame.S(frame.StyleNone, gutterGap),
			frame.S(frame.StyleMuted, sessionText(info)),
		))
	}

	return append(lines, hintLine(g))
}

// compact is the narrow-width fallback: the wordmark still reads, the
// affordances still list, the art is simply not worth the rows. The gutter
// gap matches the full layout's, so the device reads as the same continuous
// stroke at both sizes rather than a bullet on a list item.
func compact(info Info) []frame.Line {
	g := gutter(info.ASCII)
	return []frame.Line{
		frame.L(
			frame.S(frame.StyleBrand, g),
			frame.S(frame.StyleNone, gutterGap),
			frame.S(frame.StyleStrong, wordmark),
			frame.S(frame.StyleMuted, tagline(info.ASCII)),
		),
		frame.L(
			frame.S(frame.StyleBrand, g),
			frame.S(frame.StyleNone, gutterGap),
			frame.S(frame.StyleMuted, compactHints),
		),
	}
}

// hintLine lists the keys a first-run user cannot guess: keys unstyled (a
// literal keystroke), labels muted, joined so the whole line dims as a unit.
func hintLine(g string) frame.Line {
	l := frame.Line{
		frame.S(frame.StyleBrand, g),
		frame.S(frame.StyleNone, gutterGap),
	}
	for i, h := range fullHints {
		if i > 0 {
			l = append(l, frame.S(frame.StyleMuted, "   "))
		}
		l = append(l, frame.S(frame.StyleNone, h.key), frame.S(frame.StyleMuted, " "+h.label))
	}
	return l
}

// sessionText is the optional "what am I talking to" line. Provider and
// session are additive: a model alone is a complete answer.
func sessionText(info Info) string {
	var b strings.Builder
	b.WriteString("model ")
	b.WriteString(info.Model)
	if info.Provider != "" {
		b.WriteString(metaSep(info.ASCII))
		b.WriteString(info.Provider)
	}
	if info.Session != "" {
		b.WriteString("   session ")
		b.WriteString(info.Session)
	}
	return b.String()
}

// StatusSegment is the persistent identity: the mint beam-bar and product
// name, muted. It is the status bar's leftmost segment, never animated; the
// caller drops it whole below minimum width rather than abbreviating it.
func StatusSegment(ascii bool) frame.Line {
	return frame.L(
		frame.S(frame.StyleBrand, gutter(ascii)),
		frame.S(frame.StyleNone, " "),
		frame.S(frame.StyleMuted, identity),
	)
}

// StatusSegmentWidth is the cell width StatusSegment occupies, so the status
// bar can budget its remaining segments without rendering identity first.
func StatusSegmentWidth(ascii bool) int {
	return textwidth.Width(StatusSegment(ascii).Text())
}

func gutter(ascii bool) string {
	if ascii {
		return gutterASCII
	}
	return gutterUnicode
}

func tagline(ascii bool) string {
	if ascii {
		return taglineASCII
	}
	return taglineUnicode
}

func metaSep(ascii bool) string {
	if ascii {
		return metaSepASCII
	}
	return metaSepUnicode
}

func ellipsis(ascii bool) string {
	if ascii {
		return ellipsisASCII
	}
	return ellipsisUnicode
}

// clamp cuts l to at most width cells, rune-safely and span-wise, marking
// the cut with an ellipsis when one fits — model, provider and session
// names are caller data of unbounded length.
func clamp(l frame.Line, width int, ascii bool) frame.Line {
	if width <= 0 {
		return frame.Line{}
	}
	if textwidth.Width(l.Text()) <= width {
		return l
	}

	tail := ellipsis(ascii)
	if textwidth.Width(tail) > width {
		tail = "" // too narrow even to say "there was more"
	}
	budget := width - textwidth.Width(tail)

	out := make(frame.Line, 0, len(l)+1)
	used := 0
	for _, s := range l {
		w := textwidth.Width(s.Text)
		if used+w <= budget {
			out = append(out, s)
			used += w
			continue
		}
		if rem := budget - used; rem > 0 {
			out = append(out, frame.S(s.Style, textwidth.Truncate(s.Text, rem, "")))
		}
		break
	}
	if tail != "" {
		out = append(out, frame.S(frame.StyleMuted, tail))
	}
	return out
}

func artFor(ascii bool) [5][]artSpan {
	if ascii {
		return artASCII
	}
	return artUnicode
}

// Package brand renders beam's identity device: the fresh-session welcome
// header and the persistent status-bar identity segment.
//
// The device is the vertical gold beam-bar `▌` — the same mark that is the
// composer sigil — plus, once per session, the website logo-mark (three
// mitered beam strokes forming an open C) as three rows of block art in the
// favicon's luminance ramp. The header is printed ONCE into scrollback by
// the app, so it survives in screenshots and history and never repaints; the
// status segment is the quiet, never-animated counterpart that stays.
//
// Everything here is a pure function of (state, width) → []frame.Line: no
// terminal reads, no capability probing, no SGR. Callers decide the ASCII
// fallback (Info.ASCII) from their own Caps snapshot, which is what keeps
// this package testable at every width without a terminal.
package brand

import (
	"strings"

	"github.com/contenox/beam/internal/surfaces/beamtui/frame"
	"github.com/contenox/beam/internal/surfaces/beamtui/textwidth"
)

// CompactWidth is the narrowest width that still fits the logo-mark
// header. Below it Welcome drops the art and prints two rows: the brand
// moment is a courtesy, never a reason to mangle a narrow terminal.
const CompactWidth = 66

// Wordmark and tagline are fixed brand copy: lowercase
// "contenox", em-dash in the unicode variant and a plain hyphen in ASCII.
// Do not paraphrase either string.
const (
	wordmark       = "contenox beam"
	taglineUnicode = " — open coding harness"
	taglineASCII   = " - open coding harness"
)

// ASCIIGutter is the beam-bar a Mono terminal sees, exported so testkit's
// glyph-parity test can hold every surface's ASCII marker against the style
// package's GlyphSet in one place. Components may not import style, so the
// agreement can only be checked from outside.
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

// artUnicode is the logo-mark rasterized from website/public/logo-mark.svg's
// actual geometry: three blades — each a quarter-arc outer edge around its
// own offset center with a straight inner edge and mitered ends — arranged
// pinwheel-style around a square core, opening right. Rendered at 9x10
// pixels via half-blocks (one text row = two pixel rows), each blade in its
// favicon ramp stop, lightest at the top. The shape was produced by
// supersampled rasterization of the SVG paths, not drawn by hand; keep any
// future change faithful to the source mark the same way.
var artUnicode = [5][]artSpan{
	{{frame.StyleNone, "    "}, {frame.StyleBrandRamp1, "▄▄███"}},
	{{frame.StyleNone, "  "}, {frame.StyleBrandRamp2, "▄"}, {frame.StyleNone, " "}, {frame.StyleBrandRamp1, "▀▀▀▀▀"}},
	{{frame.StyleBrandRamp2, "▄██"}},
	{{frame.StyleBrandRamp2, "▀██"}, {frame.StyleNone, " "}, {frame.StyleBrandRamp3, "▄▄▄▄▄"}},
	{{frame.StyleNone, "    "}, {frame.StyleBrandRamp3, "▀▀███"}},
}

// artASCII suggests the same swirl in characters a legacy console can draw:
// the diagonals stand in for the arc, the gaps stay.
//
// The mark is SYMMETRIC about its middle row, because the mark it stands in
// for is: the unicode art gives the top blade an inner edge ("▀▀▀▀▀") and the
// bottom blade the mirrored one ("▄▄▄▄▄"). This column used to draw only the
// bottom one, so the ASCII device read as a lopsided hook rather than as the
// same open C every other surface shows. The two bars ride at the two heights
// ASCII has to offer — tildes near the top of their row, underscores on the
// baseline — which is as close to ▀/▄ as a legacy console gets.
var artASCII = [5][]artSpan{
	{{frame.StyleNone, "    "}, {frame.StyleBrandRamp1, ",==="}},
	{{frame.StyleNone, "   "}, {frame.StyleBrandRamp2, "/"}, {frame.StyleNone, " "}, {frame.StyleBrandRamp1, "~~~"}},
	{{frame.StyleNone, "  "}, {frame.StyleBrandRamp2, "|"}},
	{{frame.StyleNone, "   "}, {frame.StyleBrandRamp2, "\\"}, {frame.StyleNone, " "}, {frame.StyleBrandRamp3, "___"}},
	{{frame.StyleNone, "    "}, {frame.StyleBrandRamp3, "`==="}},
}

// wordmarkRow is the art row the wordmark hangs beside — the mark's
// vertical middle.
const wordmarkRow = 2

// hint is one key and what it opens. Keys print unstyled so they read as
// literal keystrokes; the label carries the muted style.
type hint struct{ key, label string }

// fullHints is the welcome header's one hint line — the four affordances a
// first-run user cannot discover by typing, plus the key list itself.
var fullHints = []hint{
	{"/", "commands"},
	{"@", "files"},
	{"!", "shell"},
	{"Ctrl+E", "editor"},
	{"?", "keys"},
}

// compactHints is the same set at narrow widths, abbreviated rather than
// truncated so every affordance survives.
const compactHints = "/ cmds  @ files  ! shell  ^E editor  ? keys"

// identity is the status bar's wordmark: the product name, not the
// surface's.
const identity = "contenox"

// Info is the session context the welcome header may show. The zero value
// renders the pure brand moment — no model, no provider, no session.
//
// ASCII selects the character fallback and must be true exactly when the
// caller's caps profile is Mono. This package never probes: the caller
// already holds the one Caps snapshot per process and passes the answer
// down, so the same header is reproducible in a test at any width.
type Info struct {
	ASCII    bool
	Model    string
	Provider string
	Session  string
}

// Welcome renders the fresh-session header for width. The app prints the
// result once into scrollback, never into the live region — it must not
// repaint, and it must survive resize as literal history.
//
// At width >= CompactWidth the layout is the logo-mark art beside the
// wordmark, an optional session line, and one hint line, all hung off a
// continuous gold gutter. Below that it collapses to wordmark + abbreviated
// hints. Either way the last line is an empty separator, so the caller
// appends the header and the first turn without inserting spacing of its
// own. No returned line is ever wider than width.
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
// affordances still list, the art is simply not worth the rows.
//
// The gutter gap is the same two cells the full layout uses. It was missing
// here, which put the wordmark hard against the beam-bar and made the device
// read as a bullet on a list item rather than as the continuous stroke the
// full layout documents — and a user narrowing their terminal watched the
// mark change meaning. Two spaces is all "one device at two sizes" costs.
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

// hintLine lists the keys a first-run user cannot guess. Keys are unstyled
// (a literal keystroke should look like one), labels are muted, and the
// separators join the muted run so the whole line dims as a unit.
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

// StatusSegment is the persistent identity: the gold beam-bar and the
// product name, muted. It is the status bar's leftmost segment, never
// animated, and the caller drops it whole below minimum width rather than
// abbreviating it.
func StatusSegment(ascii bool) frame.Line {
	return frame.L(
		frame.S(frame.StyleBrand, gutter(ascii)),
		frame.S(frame.StyleNone, " "),
		frame.S(frame.StyleMuted, identity),
	)
}

// StatusSegmentWidth is the cell width StatusSegment occupies, for the
// status bar's layout math — so the bar budgets its remaining segments
// without rendering the identity first.
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
// the cut with an ellipsis when one fits. Model, provider and session names
// are caller data of unbounded length, so this is a real bound and not just
// a defence for the compact layout.
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

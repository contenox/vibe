// Package frame is beam's rendering schema: the pure-data contract between
// components (pure functions of (state, width) -> []Line) and the terminal
// engine that draws it. Spans carry semantic StyleIDs, never escape codes,
// so golden tests compare plain data. The package is dependency-free: no
// imports beyond the standard library, no knowledge of terminals or styles.
package frame

// StyleID names a semantic style role. The style package holds the only
// mapping from StyleID to terminal attributes; the set is closed, and the
// style package's completeness test walks All.
type StyleID string

const (
	StyleNone      StyleID = ""          // default foreground
	StyleUser      StyleID = "user"      // user-authored turn text
	StyleAssistant StyleID = "assistant" // assistant prose
	StyleThought   StyleID = "thought"   // reasoning traces, dimmed
	StyleShell     StyleID = "shell"     // user shell lines and their output
	StyleTool      StyleID = "tool"      // tool-call card chrome
	StyleError     StyleID = "error"
	StyleWarn      StyleID = "warn"
	StyleMuted     StyleID = "muted"
	StyleBorder    StyleID = "border"
	StyleActive    StyleID = "active"   // focused/selected element
	StyleInactive  StyleID = "inactive" // unfocused counterpart
	StylePending   StyleID = "pending"
	StyleDone      StyleID = "done"
	StyleFailed    StyleID = "failed"
	StyleSkipped   StyleID = "skipped"
	StyleHITL      StyleID = "hitl"    // approval-card chrome
	StyleBrand     StyleID = "brand"   // brand mint; closed usage list only
	StyleHeading   StyleID = "heading" // markdown headings
	StyleEmphasis  StyleID = "em"
	StyleStrong    StyleID = "strong"
	StyleCode      StyleID = "code" // inline code and code-block text

	// The brand-mint luminance ramp: the three stops of the website
	// logo-mark, lightest to deepest, so the mark's three mitered beam
	// strokes keep their depth as block art. These belong to StyleBrand's
	// closed usage list — today the fresh-session welcome header and
	// nothing else. Never body text, never a semantic state, never a
	// background.
	StyleBrandRamp1 StyleID = "brand-ramp1" // lightest stop, top stroke
	StyleBrandRamp2 StyleID = "brand-ramp2" // mid stop, spine (== brand)
	StyleBrandRamp3 StyleID = "brand-ramp3" // deepest stop, bottom stroke
)

// All lists every StyleID. The style package's completeness test fails when
// a role here has no table entry, and components may only use IDs from this
// list.
func All() []StyleID {
	return []StyleID{
		StyleNone, StyleUser, StyleAssistant, StyleThought, StyleShell,
		StyleTool, StyleError, StyleWarn, StyleMuted, StyleBorder,
		StyleActive, StyleInactive, StylePending, StyleDone, StyleFailed,
		StyleSkipped, StyleHITL, StyleBrand, StyleHeading, StyleEmphasis,
		StyleStrong, StyleCode,
		StyleBrandRamp1, StyleBrandRamp2, StyleBrandRamp3,
	}
}

// Span is a run of text under one style. Text must never contain escape
// codes or control characters other than what the user typed; renderers
// treat it as literal cells.
type Span struct {
	Text  string
	Style StyleID
}

// Line is one terminal row's worth of spans. Lines never contain newlines;
// wrapping to a width is the producing component's job (textwidth package).
type Line []Span

// Text returns the line's unstyled text. Because spans carry no escape
// codes, this is exactly what copying the rendered line out of the
// terminal yields — the copy/paste acceptance tests rely on that identity.
func (l Line) Text() string {
	switch len(l) {
	case 0:
		return ""
	case 1:
		return l[0].Text
	}
	n := 0
	for _, s := range l {
		n += len(s.Text)
	}
	b := make([]byte, 0, n)
	for _, s := range l {
		b = append(b, s.Text...)
	}
	return string(b)
}

// Cursor is the caret position within the Live region, in 0-based cells.
type Cursor struct {
	Row    int
	Col    int
	Hidden bool
}

// Frame is one complete render handed to the engine's Commit.
//
// Scrollback holds the lines newly appended by this commit; the engine
// prints each exactly once into the terminal's real history, where they
// are resize-immune and natively selectable, and never repaints them. Live
// is the complete bounded region (composer, status bar, open overlays)
// repainted in place on every commit; its height is len(Live).
type Frame struct {
	Scrollback []Line
	Live       []Line
	Cursor     Cursor
}

// S builds a span.
func S(style StyleID, text string) Span { return Span{Text: text, Style: style} }

// L builds a line from spans.
func L(spans ...Span) Line { return Line(spans) }

// Plain builds a single-span unstyled line.
func Plain(text string) Line { return Line{Span{Text: text}} }

// Styled builds a single-span line under one style.
func Styled(style StyleID, text string) Line { return Line{Span{Text: text, Style: style}} }

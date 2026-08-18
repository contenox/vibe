package style

// GlyphSet is the fixed set of non-color visual symbols components draw
// from so meaning survives when color is unavailable or simply ignored:
// a spinner, small status markers, and expand/collapse indicators. Color
// is never the only signal — every colored role in the table above has a
// glyph or text counterpart that carries the same meaning alone.
type GlyphSet struct {
	SpinnerFrames []string
	Bullet        string
	Check         string
	Cross         string
	Ellipsis      string
	Collapsed     string
	Expanded      string
	PromptSigil   string
	GaugeFull     string
	GaugeEmpty    string
}

// unicodeGlyphs is the baseline set: a braille spinner and the usual
// prose symbols.
var unicodeGlyphs = GlyphSet{
	SpinnerFrames: []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"},
	Bullet:        "•",
	Check:         "✓",
	Cross:         "✗",
	Ellipsis:      "…",
	Collapsed:     "▸",
	Expanded:      "▾",
	PromptSigil:   "▌",
	GaugeFull:     "█",
	GaugeEmpty:    "░",
}

// asciiGlyphs is the fallback for terminals that cannot be trusted with
// unicode (legacy consoles, plain mode, CI).
//
// This column is not free to drift: components may not import this package
// and this package may not import components, so each side spells its own
// ASCII vocabulary out, and a character meaning one thing here and another
// there is a legibility bug in exactly the terminals with no color to fall
// back on. testkit's glyph-parity test holds the two together; the pairs
// it pins are noted per field.
var asciiGlyphs = GlyphSet{
	SpinnerFrames: []string{"-", "\\", "|", "/"},
	Bullet:        "*",
	Check:         "+", // == transcript.ASCIIDone, approval.ASCIIOk
	Cross:         "x", // == transcript.ASCIIFailed, approval.ASCIINo
	Ellipsis:      "...",
	Collapsed:     ">", // == transcript.ASCIIUser — the "points at" marker
	Expanded:      "v",
	// The beam-bar, not a chevron: ">" doubled as both the prompt device and
	// the collapsed marker, so the character identifying beam's input line
	// also meant "there is more under this". Every surface drawing the bar
	// already degrades to "|".
	PromptSigil: "|",
	GaugeFull:   "#",
	GaugeEmpty:  "-",
}

// Glyphs returns the glyph set for caps: the unicode baseline for every
// color-capable profile, and the ASCII fallback for ProfileMono — glyph
// decoration tracks the same detection as color rather than a separate
// probe.
func Glyphs(caps Caps) GlyphSet {
	if caps.Profile == ProfileMono {
		return asciiGlyphs
	}
	return unicodeGlyphs
}

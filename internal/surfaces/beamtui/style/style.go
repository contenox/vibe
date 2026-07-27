// Package style is beam's only StyleID→terminal-attributes table. It owns
// the tier ladder (truecolor → 256 → 16 → mono), the capability snapshot
// that selects a tier, the closed beam-gold brand ladder, and the glyph
// set that carries meaning when color is unavailable or ignored.
//
// No other package may construct an escape sequence or invent a color:
// components emit frame.StyleID values, term.Engine asks a StyleResolver
// (this package's Styles) for the SGR prefix/suffix pair, and that lookup
// is the only path from a semantic role to a terminal attribute.
package style

import "github.com/contenox/beam/internal/surfaces/beamtui/frame"

// resetSuffix ends any non-empty prefix. SGR 0 clears every attribute the
// prefix set, so spans never bleed into the text that follows them.
const resetSuffix = "\x1b[0m"

// Attribute-only SGR codes. These carry the same meaning at every color
// tier, so they need no per-profile variant the way colors do.
const (
	attrBold   = "\x1b[1m"
	attrDim    = "\x1b[2m"
	attrItalic = "\x1b[3m"
)

// ANSI16 foreground codes. The tier is the aixterm 16-color set (8
// standard + 8 bright); bright variants are used throughout for the
// "16-color readable" half of the tier doctrine.
const (
	fg16Red     = "\x1b[91m"
	fg16Green   = "\x1b[92m"
	fg16Yellow  = "\x1b[93m"
	fg16Blue    = "\x1b[94m"
	fg16Magenta = "\x1b[95m"
	fg16Cyan    = "\x1b[96m"
	fg16Gray    = "\x1b[90m"
)

// ANSI256 foreground codes, indexed into the standard xterm 256-color
// palette (\x1b[38;5;Nm).
const (
	fg256Red     = "\x1b[38;5;203m"
	fg256Yellow  = "\x1b[38;5;221m"
	fg256Green   = "\x1b[38;5;114m"
	fg256Cyan    = "\x1b[38;5;116m"
	fg256Magenta = "\x1b[38;5;176m"
	fg256Gray    = "\x1b[38;5;244m"
	fg256Code    = "\x1b[38;5;110m"
	fg256Brand   = "\x1b[38;5;214m" // brand ladder, fixed for both themes

	// Logo-mark luminance ramp, fixed for both themes (221/214/208): the
	// 256-color palette has no separate light-terminal amber run worth
	// splitting, and 214 keeps the mid stop identical to fg256Brand.
	fg256Ramp1 = "\x1b[38;5;221m"
	fg256Ramp2 = "\x1b[38;5;214m"
	fg256Ramp3 = "\x1b[38;5;208m"
)

// TrueColor foreground codes (\x1b[38;2;R;G;Bm).
const (
	fgTCRed        = "\x1b[38;2;248;113;113m"
	fgTCYellow     = "\x1b[38;2;250;204;21m"
	fgTCGreen      = "\x1b[38;2;74;222;128m"
	fgTCCyan       = "\x1b[38;2;34;211;238m"
	fgTCMagenta    = "\x1b[38;2;192;132;252m"
	fgTCGray       = "\x1b[38;2;107;114;128m"
	fgTCCode       = "\x1b[38;2;125;211;252m"
	fgTCBrandDark  = "\x1b[38;2;251;191;36m" // beam gold, dark terminal (#FBBF24)
	fgTCBrandLight = "\x1b[38;2;180;83;9m"   // beam gold, light terminal (#B45309)

	// Logo-mark luminance ramp, lightest to deepest, mirroring
	// website/public/favicon.svg exactly. The mid dark stop IS beam gold
	// and the lightest light stop IS the light-terminal brand, so the
	// mark reads as one family with the rest of the accent.
	fgTCRamp1Dark  = "\x1b[38;2;252;211;77m" // #FCD34D
	fgTCRamp2Dark  = "\x1b[38;2;251;191;36m" // #FBBF24
	fgTCRamp3Dark  = "\x1b[38;2;245;158;11m" // #F59E0B
	fgTCRamp1Light = "\x1b[38;2;180;83;9m"   // #B45309
	fgTCRamp2Light = "\x1b[38;2;217;119;6m"  // #D97706
	fgTCRamp3Light = "\x1b[38;2;245;158;11m" // #F59E0B
)

// Styles is the process-lifetime StyleID→SGR table for one Caps snapshot.
// It satisfies term.StyleResolver; construct exactly one per process via
// New and hand it to the terminal engine.
type Styles struct {
	table map[frame.StyleID]string
}

// New builds the resolver for caps. The role table is fixed at
// construction time — Styles never re-reads the environment or re-probes
// the terminal.
func New(caps Caps) *Styles {
	return &Styles{table: buildTable(caps)}
}

// SGR returns the SGR prefix/suffix pair for id. prefix is a single SGR
// sequence or empty; suffix is the reset sequence whenever prefix is
// non-empty, and empty otherwise. Every frame.StyleID resolves — an id
// missing from the table (impossible for the closed set in frame.All,
// but SGR must never panic on one) degrades to the empty pair, same as
// Mono.
func (s *Styles) SGR(id frame.StyleID) (prefix, suffix string) {
	prefix = s.table[id]
	if prefix == "" {
		return "", ""
	}
	return prefix, resetSuffix
}

// buildTable returns the role table for caps. Mono returns nil: every
// lookup on a nil map yields the zero value, so SGR strips ALL styling
// without a second code path — this is the doctrine, not an
// optimization.
//
// Role values (the same across Dark/light except brand, which is the
// one role the blueprint fixes a light/dark ladder for):
//
//	none, assistant, shell            empty (default foreground)
//	user, heading, strong, active     bold
//	em                                italic
//	thought, muted, skipped           dim
//	error, failed                     red
//	warn                              yellow
//	done                              green
//	pending                           cyan
//	code                              soft cyan/blue
//	hitl                              magenta
//	border, inactive, tool            bright-black (chrome)
//	brand                             gold ladder; ANSI16 = bold, no color
//	brand-ramp1/2/3                   logo-mark gold ramp, same ladder rules
//
// Every prefix here is foreground/attribute-only: no role ever emits a
// background or reverse-video code, in content or chrome (V1 has no
// never-copied chrome exception yet — see the package doc).
func buildTable(caps Caps) map[frame.StyleID]string {
	if caps.Profile == ProfileMono {
		return nil
	}

	t := map[frame.StyleID]string{
		frame.StyleNone:      "",
		frame.StyleAssistant: "",
		frame.StyleShell:     "",
		frame.StyleUser:      attrBold,
		frame.StyleHeading:   attrBold,
		frame.StyleStrong:    attrBold,
		frame.StyleActive:    attrBold,
		frame.StyleEmphasis:  attrItalic,
		frame.StyleThought:   attrDim,
		frame.StyleMuted:     attrDim,
		frame.StyleSkipped:   attrDim,
	}

	var red, yellow, green, cyan, magenta, gray, code, brand string
	var ramp1, ramp2, ramp3 string
	switch caps.Profile {
	case ProfileANSI16:
		red, yellow, green, cyan, magenta, gray, code = fg16Red, fg16Yellow, fg16Green, fg16Cyan, fg16Magenta, fg16Gray, fg16Blue
		// Brand degrades to emphasis only — never a wrong-looking color.
		// The ramp collapses with it: three near-identical golds have no
		// 16-color spelling, so the logo-mark art reads as one bold shape.
		brand = attrBold
		ramp1, ramp2, ramp3 = attrBold, attrBold, attrBold
	case ProfileANSI256:
		red, yellow, green, cyan, magenta, gray, code = fg256Red, fg256Yellow, fg256Green, fg256Cyan, fg256Magenta, fg256Gray, fg256Code
		brand = fg256Brand
		ramp1, ramp2, ramp3 = fg256Ramp1, fg256Ramp2, fg256Ramp3
	case ProfileTrueColor:
		red, yellow, green, cyan, magenta, gray, code = fgTCRed, fgTCYellow, fgTCGreen, fgTCCyan, fgTCMagenta, fgTCGray, fgTCCode
		if caps.Dark {
			brand = fgTCBrandDark
			ramp1, ramp2, ramp3 = fgTCRamp1Dark, fgTCRamp2Dark, fgTCRamp3Dark
		} else {
			brand = fgTCBrandLight
			ramp1, ramp2, ramp3 = fgTCRamp1Light, fgTCRamp2Light, fgTCRamp3Light
		}
	}

	t[frame.StyleError] = red
	t[frame.StyleFailed] = red
	t[frame.StyleWarn] = yellow
	t[frame.StyleDone] = green
	t[frame.StylePending] = cyan
	t[frame.StyleHITL] = magenta
	t[frame.StyleBorder] = gray
	t[frame.StyleInactive] = gray
	t[frame.StyleTool] = gray
	t[frame.StyleCode] = code
	t[frame.StyleBrand] = brand
	t[frame.StyleBrandRamp1] = ramp1
	t[frame.StyleBrandRamp2] = ramp2
	t[frame.StyleBrandRamp3] = ramp3

	return t
}

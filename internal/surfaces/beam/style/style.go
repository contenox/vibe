// Package style is beam's only StyleID→terminal-attributes table: the
// tier ladder (truecolor → 256 → 16 → mono), the capability snapshot that
// selects a tier, the brand ladder, and the glyph set that carries meaning
// when color is unavailable. No other package may construct an escape
// sequence or invent a color; Styles is the only path from role to attribute.
package style

import "github.com/contenox/contenox/internal/surfaces/beam/frame"

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

	// Brand mint ladder: unlike the old amber ladder's near-identical
	// shades, mint has a real 16-color spelling. The luminous rung gets
	// the same bright green every other bright-tier role uses; the core
	// and dim rungs — indistinguishable at this tier — collapse onto the
	// one plain green the palette offers, a true color rather than a
	// bold-only fallback.
	fg16BrandRamp1 = "\x1b[92m"
	fg16BrandCore  = "\x1b[32m"
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
	fg256Brand   = "\x1b[38;5;78m" // brand ladder, fixed for both themes

	// Logo-mark luminance ramp, fixed for both themes (85/78/29): the
	// 256-color palette has no separate light-terminal mint run worth
	// splitting, and 78 keeps the mid stop identical to fg256Brand. Each
	// code is the nearest xterm 6x6x6-cube color to its truecolor rung
	// (85 = #5FFFAF exactly; 78 = #5FD787, nearest to #34D399; 29 =
	// #00875F, nearest to #059669).
	fg256Ramp1 = "\x1b[38;5;85m"
	fg256Ramp2 = "\x1b[38;5;78m"
	fg256Ramp3 = "\x1b[38;5;29m"
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
	fgTCBrandDark  = "\x1b[38;2;52;211;153m" // brand mint, dark terminal (#34D399)
	fgTCBrandLight = "\x1b[38;2;5;150;105m"  // brand mint, light terminal (#059669)

	// Logo-mark luminance ramp, lightest to deepest: the terminal's own
	// three-rung mint ladder (luminous/core/dim), tuned for tier fidelity
	// — ramp1 is an exact xterm 256-cube color — rather than byte-parity
	// with the website's mint tokens. The mid dark stop is brand mint and
	// the lightest light stop is the light-terminal brand, so the mark
	// reads as one family with the rest of the accent.
	fgTCRamp1Dark  = "\x1b[38;2;95;255;175m" // #5FFFAF
	fgTCRamp2Dark  = "\x1b[38;2;52;211;153m" // #34D399
	fgTCRamp3Dark  = "\x1b[38;2;5;150;105m"  // #059669
	fgTCRamp1Light = "\x1b[38;2;5;150;105m"  // #059669
	fgTCRamp2Light = "\x1b[38;2;52;211;153m" // #34D399
	fgTCRamp3Light = "\x1b[38;2;95;255;175m" // #5FFFAF
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
// lookup on a nil map yields the zero value, so SGR strips all styling
// without a second code path — this is the doctrine, not an optimization.
//
// Role values (the same across Dark/light except brand, which is the only
// role with a light/dark ladder):
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
//	brand                             brand-mint ladder; ANSI16 = green
//	brand-ramp1/2/3                   logo-mark mint ramp, same ladder rules
//
// Every prefix here is foreground/attribute-only: no role ever emits a
// background or reverse-video code, in content or chrome.
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
		// Mint has a real 16-color spelling, unlike the old amber
		// ladder's near-identical shades: the luminous rung gets its own
		// bright green, and the core/dim rungs — indistinguishable at
		// this tier — share the one plain green left, a true color
		// rather than a bold-only fallback.
		brand = fg16BrandCore
		ramp1, ramp2, ramp3 = fg16BrandRamp1, fg16BrandCore, fg16BrandCore
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

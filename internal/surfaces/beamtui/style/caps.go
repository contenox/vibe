package style

import (
	"os"
	"strings"
)

// Profile is a terminal's color capability tier, ordered from least to
// most capable. The zero value is ProfileMono, so a zero-value Caps
// degrades safely without an explicit Detect call.
type Profile int

const (
	// ProfileMono drops all styling: no color, no bold, no italic. This
	// is doctrine, not a fallback of convenience — NO_COLOR, TERM=dumb,
	// and non-tty output all land here and every SGR call returns the
	// empty pair.
	ProfileMono Profile = iota
	// ProfileANSI16 is the aixterm 16-color set (8 standard + 8 bright).
	ProfileANSI16
	// ProfileANSI256 is the xterm 256-color palette.
	ProfileANSI256
	// ProfileTrueColor is 24-bit RGB.
	ProfileTrueColor
)

// Caps is a process-lifetime capability snapshot: which color tier the
// attached terminal supports, and which background it presumably sits
// on. Detect (or DetectFromOS) produces exactly one Caps per process —
// nothing in this package re-probes the terminal afterward.
type Caps struct {
	Profile Profile
	Dark    bool
}

// Detect derives a Caps snapshot purely from the injected env accessor
// and tty flag; it never reads the environment or the terminal itself,
// which is what makes every rule below unit-testable without touching
// the process.
//
// Profile rules, in order:
//   - not a tty, NO_COLOR set to any value, or TERM=dumb: ProfileMono.
//     This is the one rule with no exception: mono strips ALL styling.
//   - COLORTERM is "truecolor" or "24bit": ProfileTrueColor.
//   - TERM contains "256color": ProfileANSI256.
//   - otherwise: ProfileANSI16.
//
// getenv returning "" is treated as "unset" — the func(string) string
// shape can't distinguish an empty-but-set NO_COLOR from an absent one,
// so any non-empty value opts out of color, matching every other NO_COLOR
// consumer's convention.
//
// Dark defaults to true (D40, the fallback when detection is
// inconclusive). BEAM_THEME=light is the only way to get false;
// BEAM_THEME=dark is accepted for symmetry but does not change the
// default, and any other value (including unset) leaves Dark true.
func Detect(getenv func(string) string, isTTY bool) Caps {
	dark := true
	switch getenv("BEAM_THEME") {
	case "light":
		dark = false
	case "dark":
		dark = true
	}

	if !isTTY || getenv("NO_COLOR") != "" || getenv("TERM") == "dumb" {
		return Caps{Profile: ProfileMono, Dark: dark}
	}

	switch getenv("COLORTERM") {
	case "truecolor", "24bit":
		return Caps{Profile: ProfileTrueColor, Dark: dark}
	}
	if strings.Contains(getenv("TERM"), "256color") {
		return Caps{Profile: ProfileANSI256, Dark: dark}
	}
	return Caps{Profile: ProfileANSI16, Dark: dark}
}

// DetectFromOS is Detect wired to the real process environment. The
// caller supplies isTTY — the term engine is the only package allowed to
// probe the terminal, and the composition root has that answer anyway.
// Call it exactly once at startup; every other caller should take the
// resulting Caps as a parameter instead of probing again.
func DetectFromOS(isTTY bool) Caps {
	return Detect(os.Getenv, isTTY)
}

package style

import (
	"strconv"
	"strings"
	"testing"

	"github.com/contenox/contenox/internal/surfaces/beam/frame"
	"github.com/contenox/contenox/internal/surfaces/beam/term"
)

// Styles must satisfy the terminal engine's resolver seam.
var _ term.StyleResolver = (*Styles)(nil)

func allProfiles() []Profile {
	return []Profile{ProfileMono, ProfileANSI16, ProfileANSI256, ProfileTrueColor}
}

func TestUnit_ResolvesWithoutPanicForEveryStyle(t *testing.T) {
	for _, p := range allProfiles() {
		for _, dark := range []bool{true, false} {
			s := New(Caps{Profile: p, Dark: dark})
			for _, id := range frame.All() {
				prefix, suffix := s.SGR(id)
				if prefix == "" && suffix != "" {
					t.Fatalf("profile %v dark=%v role %q: empty prefix, non-empty suffix %q", p, dark, id, suffix)
				}
				if prefix != "" && suffix != resetSuffix {
					t.Fatalf("profile %v dark=%v role %q: prefix %q, suffix %q, want reset", p, dark, id, prefix, suffix)
				}
			}
		}
	}
}

func TestUnit_MonoStripsAllStyling(t *testing.T) {
	for _, dark := range []bool{true, false} {
		s := New(Caps{Profile: ProfileMono, Dark: dark})
		for _, id := range frame.All() {
			prefix, suffix := s.SGR(id)
			if prefix != "" || suffix != "" {
				t.Fatalf("Mono dark=%v role %q = (%q, %q), want empty pair", dark, id, prefix, suffix)
			}
		}
	}
}

func TestUnit_BrandLadderExactBytes(t *testing.T) {
	cases := []struct {
		name   string
		caps   Caps
		prefix string
	}{
		{"truecolor dark", Caps{Profile: ProfileTrueColor, Dark: true}, "\x1b[38;2;52;211;153m"},
		{"truecolor light", Caps{Profile: ProfileTrueColor, Dark: false}, "\x1b[38;2;5;150;105m"},
		{"ansi256 dark", Caps{Profile: ProfileANSI256, Dark: true}, "\x1b[38;5;78m"},
		{"ansi256 light", Caps{Profile: ProfileANSI256, Dark: false}, "\x1b[38;5;78m"},
		{"ansi16 dark", Caps{Profile: ProfileANSI16, Dark: true}, "\x1b[32m"},
		{"ansi16 light", Caps{Profile: ProfileANSI16, Dark: false}, "\x1b[32m"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := New(c.caps)
			prefix, suffix := s.SGR(frame.StyleBrand)
			if prefix != c.prefix {
				t.Fatalf("brand prefix = %q, want %q", prefix, c.prefix)
			}
			if suffix != resetSuffix {
				t.Fatalf("brand suffix = %q, want %q", suffix, resetSuffix)
			}
		})
	}

	// The 16-color tier must use a real basic color, never an extended
	// truecolor/256 approximation escaping the tier boundary.
	s := New(Caps{Profile: ProfileANSI16})
	prefix, _ := s.SGR(frame.StyleBrand)
	if strings.Contains(prefix, "38;") {
		t.Fatalf("ANSI16 brand prefix %q must not carry an extended-color code", prefix)
	}
}

// TestUnit_BrandRampLadderExactBytes pins the logo-mark ramp to the
// terminal's own mint ladder byte for byte, since goldens compare
// StyleIDs, not colors, and wouldn't catch a drift here.
func TestUnit_BrandRampLadderExactBytes(t *testing.T) {
	cases := []struct {
		name  string
		caps  Caps
		ramp1 string
		ramp2 string
		ramp3 string
	}{
		{
			"truecolor dark", Caps{Profile: ProfileTrueColor, Dark: true},
			"\x1b[38;2;95;255;175m", // #5FFFAF
			"\x1b[38;2;52;211;153m", // #34D399
			"\x1b[38;2;5;150;105m",  // #059669
		},
		{
			"truecolor light", Caps{Profile: ProfileTrueColor, Dark: false},
			"\x1b[38;2;5;150;105m",  // #059669
			"\x1b[38;2;52;211;153m", // #34D399
			"\x1b[38;2;95;255;175m", // #5FFFAF
		},
		{
			"ansi256 dark", Caps{Profile: ProfileANSI256, Dark: true},
			"\x1b[38;5;85m", "\x1b[38;5;78m", "\x1b[38;5;29m",
		},
		{
			"ansi256 light", Caps{Profile: ProfileANSI256, Dark: false},
			"\x1b[38;5;85m", "\x1b[38;5;78m", "\x1b[38;5;29m",
		},
		{"ansi16 dark", Caps{Profile: ProfileANSI16, Dark: true}, "\x1b[92m", "\x1b[32m", "\x1b[32m"},
		{"ansi16 light", Caps{Profile: ProfileANSI16, Dark: false}, "\x1b[92m", "\x1b[32m", "\x1b[32m"},
		{"mono dark", Caps{Profile: ProfileMono, Dark: true}, "", "", ""},
		{"mono light", Caps{Profile: ProfileMono, Dark: false}, "", "", ""},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := New(c.caps)
			for _, want := range []struct {
				id     frame.StyleID
				prefix string
			}{
				{frame.StyleBrandRamp1, c.ramp1},
				{frame.StyleBrandRamp2, c.ramp2},
				{frame.StyleBrandRamp3, c.ramp3},
			} {
				prefix, _ := s.SGR(want.id)
				if prefix != want.prefix {
					t.Fatalf("%s prefix = %q, want %q", want.id, prefix, want.prefix)
				}
			}
		})
	}

	// The mid dark stop is brand mint: the mark must sit in the same
	// family as the spinner, sigil and status identity, not next to it.
	dark := New(Caps{Profile: ProfileTrueColor, Dark: true})
	mid, _ := dark.SGR(frame.StyleBrandRamp2)
	brand, _ := dark.SGR(frame.StyleBrand)
	if mid != "\x1b[38;2;52;211;153m" || mid != brand {
		t.Fatalf("ramp2 dark = %q, want %q and equal to brand %q", mid, "\x1b[38;2;52;211;153m", brand)
	}

	// Same 16-color doctrine as brand: a real basic color, never an
	// extended approximation.
	a16 := New(Caps{Profile: ProfileANSI16})
	for _, id := range []frame.StyleID{frame.StyleBrandRamp1, frame.StyleBrandRamp2, frame.StyleBrandRamp3} {
		prefix, _ := a16.SGR(id)
		if strings.Contains(prefix, "38;") {
			t.Fatalf("ANSI16 %s prefix %q must not carry an extended-color code", id, prefix)
		}
	}
}

// sgrCodes parses the numeric SGI codes out of a single SGR sequence
// ("\x1b[...m"), failing the test if prefix is not shaped that way.
func sgrCodes(t *testing.T, prefix string) []int {
	t.Helper()
	if prefix == "" {
		return nil
	}
	if !strings.HasPrefix(prefix, "\x1b[") || !strings.HasSuffix(prefix, "m") {
		t.Fatalf("prefix %q is not a single SGR sequence", prefix)
	}
	body := strings.TrimSuffix(strings.TrimPrefix(prefix, "\x1b["), "m")
	parts := strings.Split(body, ";")
	codes := make([]int, 0, len(parts))
	for _, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil {
			t.Fatalf("prefix %q has non-numeric SGR code %q", prefix, p)
		}
		codes = append(codes, n)
	}
	return codes
}

// hasDisallowedCode walks codes as SGR parameters, treating the extended
// foreground introducer (38;5;N or 38;2;R;G;B) as one unit so its color
// components aren't mistaken for standalone background/reverse codes. It
// reports true on an actual background color (40-47, 100-107, the 48
// extended introducer) or reverse video (7).
func hasDisallowedCode(codes []int) bool {
	for i := 0; i < len(codes); i++ {
		c := codes[i]
		switch {
		case c == 38:
			// Extended FOREGROUND color: skip its sub-parameters so they
			// are never re-interpreted as top-level SGR codes.
			if i+1 < len(codes) {
				switch codes[i+1] {
				case 5:
					i += 2 // "5", palette index
				case 2:
					i += 4 // "2", R, G, B
				}
			}
		case c == 48, c == 7, c >= 40 && c <= 47, c >= 100 && c <= 107:
			return true
		}
	}
	return false
}

// TestUnit_ForegroundOnlyAcrossAllRoles pins that no role's prefix carries
// a background color (40-47, 100-107, the 48 extended introducer) or
// reverse video (7).
func TestUnit_ForegroundOnlyAcrossAllRoles(t *testing.T) {
	for _, p := range []Profile{ProfileANSI16, ProfileANSI256, ProfileTrueColor} {
		for _, dark := range []bool{true, false} {
			s := New(Caps{Profile: p, Dark: dark})
			for _, id := range frame.All() {
				prefix, _ := s.SGR(id)
				if hasDisallowedCode(sgrCodes(t, prefix)) {
					t.Fatalf("profile %v dark=%v role %q: prefix %q carries a background or reverse-video code", p, dark, id, prefix)
				}
			}
		}
	}
}

func TestUnit_Detect(t *testing.T) {
	env := func(m map[string]string) func(string) string {
		return func(k string) string { return m[k] }
	}

	cases := []struct {
		name        string
		env         map[string]string
		isTTY       bool
		wantProfile Profile
		wantDark    bool
	}{
		{"not a tty", map[string]string{"TERM": "xterm-256color"}, false, ProfileMono, true},
		{"NO_COLOR set truthy", map[string]string{"NO_COLOR": "1"}, true, ProfileMono, true},
		{"NO_COLOR set to any value still counts", map[string]string{"NO_COLOR": "0"}, true, ProfileMono, true},
		{"TERM dumb", map[string]string{"TERM": "dumb"}, true, ProfileMono, true},
		{"COLORTERM truecolor", map[string]string{"COLORTERM": "truecolor", "TERM": "xterm"}, true, ProfileTrueColor, true},
		{"COLORTERM 24bit", map[string]string{"COLORTERM": "24bit", "TERM": "xterm"}, true, ProfileTrueColor, true},
		{"TERM 256color", map[string]string{"TERM": "xterm-256color"}, true, ProfileANSI256, true},
		{"plain tty defaults to ansi16", map[string]string{"TERM": "xterm"}, true, ProfileANSI16, true},
		{"no env at all still resolves ansi16", map[string]string{}, true, ProfileANSI16, true},
		{"BEAM_THEME light", map[string]string{"TERM": "xterm", "BEAM_THEME": "light"}, true, ProfileANSI16, false},
		{"BEAM_THEME dark explicit", map[string]string{"TERM": "xterm", "BEAM_THEME": "dark"}, true, ProfileANSI16, true},
		{"BEAM_THEME unset defaults dark", map[string]string{"TERM": "xterm"}, true, ProfileANSI16, true},
		{"BEAM_THEME garbage defaults dark", map[string]string{"TERM": "xterm", "BEAM_THEME": "sepia"}, true, ProfileANSI16, true},
		{"NO_COLOR wins over COLORTERM", map[string]string{"NO_COLOR": "1", "COLORTERM": "truecolor"}, true, ProfileMono, true},
		{"TERM dumb wins over COLORTERM", map[string]string{"TERM": "dumb", "COLORTERM": "truecolor"}, true, ProfileMono, true},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := Detect(env(c.env), c.isTTY)
			if got.Profile != c.wantProfile {
				t.Errorf("Profile = %v, want %v", got.Profile, c.wantProfile)
			}
			if got.Dark != c.wantDark {
				t.Errorf("Dark = %v, want %v", got.Dark, c.wantDark)
			}
		})
	}
}

func TestUnit_GlyphsASCIIFallbackInMono(t *testing.T) {
	ascii := Glyphs(Caps{Profile: ProfileMono})
	fields := append([]string{
		ascii.Bullet, ascii.Check, ascii.Cross, ascii.Ellipsis,
		ascii.Collapsed, ascii.Expanded, ascii.PromptSigil,
		ascii.GaugeFull, ascii.GaugeEmpty,
	}, ascii.SpinnerFrames...)
	for _, s := range fields {
		for _, r := range s {
			if r > 127 {
				t.Fatalf("Mono glyph set contains non-ASCII rune %q in %q", r, s)
			}
		}
	}

	for _, p := range []Profile{ProfileANSI16, ProfileANSI256, ProfileTrueColor} {
		unicodeSet := Glyphs(Caps{Profile: p})
		if unicodeSet.Collapsed == ascii.Collapsed || unicodeSet.PromptSigil == ascii.PromptSigil {
			t.Fatalf("profile %v: expected unicode glyph set to differ from ASCII fallback", p)
		}
		if len(unicodeSet.SpinnerFrames) == 0 {
			t.Fatalf("profile %v: SpinnerFrames must not be empty", p)
		}
	}
}

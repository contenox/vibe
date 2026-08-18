package testkit

import (
	"testing"

	"github.com/contenox/contenox/internal/surfaces/beam/comp/approval"
	"github.com/contenox/contenox/internal/surfaces/beam/comp/brand"
	"github.com/contenox/contenox/internal/surfaces/beam/comp/composer"
	"github.com/contenox/contenox/internal/surfaces/beam/comp/palette"
	"github.com/contenox/contenox/internal/surfaces/beam/comp/picker"
	"github.com/contenox/contenox/internal/surfaces/beam/comp/transcript"
	"github.com/contenox/contenox/internal/surfaces/beam/style"
)

// TestUnit_ASCIIGlyphParity pins that style's Mono glyphs and each
// component's own ASCII constants agree, since the style/components import
// boundary keeps the compiler from catching a mismatch itself.
func TestUnit_ASCIIGlyphParity(t *testing.T) {
	g := style.Glyphs(style.Caps{Profile: style.ProfileMono})

	pairs := []struct {
		role  string
		style string
		comps map[string]string
	}{
		{
			role:  "check — a thing that finished successfully",
			style: g.Check,
			comps: map[string]string{
				"transcript.ASCIIDone": transcript.ASCIIDone,
				"approval.ASCIIOk":     approval.ASCIIOk,
			},
		},
		{
			role:  "cross — a thing that failed",
			style: g.Cross,
			comps: map[string]string{
				"transcript.ASCIIFailed": transcript.ASCIIFailed,
				"approval.ASCIINo":       approval.ASCIINo,
			},
		},
		{
			role:  "collapsed — points at content, does not carry it",
			style: g.Collapsed,
			comps: map[string]string{
				"transcript.ASCIIUser": transcript.ASCIIUser,
			},
		},
		{
			role:  "prompt sigil — the beam-bar, beam's own device",
			style: g.PromptSigil,
			comps: map[string]string{
				"composer.ASCIISigil": composer.ASCIISigil,
				"palette.ASCIIMarker": palette.ASCIIMarker,
				"picker.ASCIIMarker":  picker.ASCIIMarker,
				"brand.ASCIIGutter":   brand.ASCIIGutter,
			},
		},
	}

	for _, p := range pairs {
		t.Run(p.role, func(t *testing.T) {
			if p.style == "" {
				t.Fatalf("style's ASCII glyph for %q is empty", p.role)
			}
			for name, got := range p.comps {
				if got != p.style {
					t.Errorf("%s = %q, but style.Glyphs(mono).%s is %q — the two ASCII sets disagree on %q",
						name, got, "(this role)", p.style, p.role)
				}
			}
		})
	}

	// A terminal with no color can't tell these apart if they're the same rune.
	if g.PromptSigil == g.Collapsed {
		t.Errorf("the ASCII prompt sigil and collapsed marker are both %q", g.PromptSigil)
	}
	if g.Check == g.Cross {
		t.Errorf("the ASCII check and cross are both %q", g.Check)
	}
}

// TestUnit_ASCIIGlyphsAreASCII pins that every Mono-profile glyph and ASCII
// constant is plain ASCII with no control runes.
func TestUnit_ASCIIGlyphsAreASCII(t *testing.T) {
	g := style.Glyphs(style.Caps{Profile: style.ProfileMono})
	all := append([]string{
		g.Bullet, g.Check, g.Cross, g.Ellipsis, g.Collapsed,
		g.Expanded, g.PromptSigil, g.GaugeFull, g.GaugeEmpty,
	}, g.SpinnerFrames...)
	all = append(all,
		transcript.ASCIIDone, transcript.ASCIIFailed, transcript.ASCIIUser,
		approval.ASCIIOk, approval.ASCIINo,
		composer.ASCIISigil, palette.ASCIIMarker, picker.ASCIIMarker, brand.ASCIIGutter,
	)
	for _, s := range all {
		for _, r := range s {
			if r > 0x7f {
				t.Errorf("ASCII glyph %q contains %U", s, r)
			}
			if r < 0x20 || r == 0x7f {
				t.Errorf("ASCII glyph %q contains control rune %U", s, r)
			}
		}
	}
}

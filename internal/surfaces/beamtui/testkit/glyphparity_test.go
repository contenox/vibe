package testkit

import (
	"testing"

	"github.com/contenox/beam/internal/surfaces/beamtui/comp/approval"
	"github.com/contenox/beam/internal/surfaces/beamtui/comp/brand"
	"github.com/contenox/beam/internal/surfaces/beamtui/comp/composer"
	"github.com/contenox/beam/internal/surfaces/beamtui/comp/palette"
	"github.com/contenox/beam/internal/surfaces/beamtui/comp/picker"
	"github.com/contenox/beam/internal/surfaces/beamtui/comp/transcript"
	"github.com/contenox/beam/internal/surfaces/beamtui/style"
)

// TestUnit_ASCIIGlyphParity holds beam's two ASCII vocabularies together.
//
// The blueprint's import boundaries make this a test's job and nobody else's:
// components may not import beamtui/style (rule c — a renderer of
// (state, width) -> frame.Line has no business owning terminal attributes),
// and style may not import components. So each side spells its own ASCII
// column out, and nothing in the compiler notices when they disagree. In a
// Mono terminal those characters ARE the vocabulary — there is no color left
// to carry the meaning — so "+" meaning "completed" on a tool card and
// something else in a status glyph is a legibility defect, not a nit.
//
// Living here is what makes it possible at all: testkit is the one package
// under beamtui that may import both sides. It is safe because the imports
// are test-only in one direction — the components' own tests import testkit's
// NON-test files, and this file is compiled only into testkit's own test
// binary, so there is no cycle either way.
//
// The pairs are the closed list. Adding a glyph to either side without adding
// it here is not caught automatically; this table is the deliberate inventory.
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

	// The device must not double as the marker that means "there is more
	// under this": one identifies beam's input line, the other is a
	// disclosure hint, and a terminal with no color cannot tell them apart.
	if g.PromptSigil == g.Collapsed {
		t.Errorf("the ASCII prompt sigil and collapsed marker are both %q", g.PromptSigil)
	}
	if g.Check == g.Cross {
		t.Errorf("the ASCII check and cross are both %q", g.Check)
	}
}

// TestUnit_ASCIIGlyphsAreASCII: the fallback exists for terminals that cannot
// draw the unicode set, so not one rune above 0x7f may hide in it.
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

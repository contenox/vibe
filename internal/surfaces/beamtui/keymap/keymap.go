// Package keymap is beam's sole key arbiter. No component ever switches on
// a raw key: every binding is declared up front ({ID, Owner, Keys, Scope,
// Help}), a chord collision between simultaneously-reachable scopes fails
// at registration time (the enforcement seam — see Registry.Register), and
// the only way a component learns "the user pressed X" is the semantic
// Action a resolved chord produces. The help overlay is generated 100% from
// registrations (Registry.Help): it contains zero hardcoded strings.
//
// This package is the direct fix for the scattered `switch msg.String()`
// anti-pattern the predecessor TUI shipped, and depends on nothing beyond
// the standard library plus beamtui/input for KeyEvent.
package keymap

import (
	"strings"
	"unicode"

	"github.com/contenox/beam/internal/surfaces/beamtui/input"
)

// Scope names one keybinding context: the global catch-all, a focusable
// pane, or a modal. Scope is intentionally an open string type — later
// components (file-tree, later panes) declare their own pane scopes without
// this package's involvement — but two values are load-bearing to Register
// and Resolve's collision/reachability rules: ScopeGlobal, and the modal
// set recognized by IsModal. Every other value behaves as a generic pane
// scope: reachable simultaneously with ScopeGlobal, but never with another
// pane scope (only one pane is focused at a time — see FocusManager).
type Scope string

// Predeclared scopes. ScopeComposer and ScopeTranscript are ordinary pane
// scopes with no special handling in this package beyond what any pane
// scope gets. ScopePalette, ScopeApproval, ScopePicker, and ScopeHelp are
// modal scopes (see IsModal): while one is open it suspends the focused
// pane and ScopeGlobal entirely, except the two reserved chords, which
// stay live under every modal.
const (
	ScopeGlobal     Scope = "global"
	ScopeComposer   Scope = "composer"
	ScopeTranscript Scope = "transcript"
	ScopePalette    Scope = "palette"
	ScopeApproval   Scope = "approval"
	ScopePicker     Scope = "picker"
	ScopeHelp       Scope = "help"
)

// modalScopes is the closed set of modal scopes. It is closed deliberately:
// a modal scope is reachable only alone (plus the reserved chords passing
// through Global), which is a stronger isolation guarantee than an
// arbitrary pane scope gets, so the set cannot grow by a caller simply
// using a new Scope string the way pane scopes can.
var modalScopes = map[Scope]bool{
	ScopePalette:  true,
	ScopeApproval: true,
	ScopePicker:   true,
	ScopeHelp:     true,
}

// IsModal reports whether s is one of the predeclared modal scopes.
func IsModal(s Scope) bool { return modalScopes[s] }

// reachable reports whether bindings in scopes a and b could ever be live
// at the same time, which is exactly when a chord collision between them
// would be ambiguous to the user. The rule (blueprint 4.5): the same scope
// always collides with itself; one global binding and one pane binding
// collide (Global is reachable whenever any pane is focused); two
// different pane scopes never collide (only one pane is focused at once);
// a modal scope collides only with itself (it is reachable only alone —
// the reserved-chord passthrough is Resolve's runtime concern, not a
// static reachability fact, and is enforced separately in Register via the
// reserved-owner check).
func reachable(a, b Scope) bool {
	if a == b {
		return true
	}
	if a == ScopeGlobal {
		return !IsModal(b)
	}
	if b == ScopeGlobal {
		return !IsModal(a)
	}
	return false
}

// Chord is the canonical text form of one keystroke, produced by ChordOf,
// or written as a literal by a Binding declaration. The grammar is closed
// and deliberately small (blueprint 4.5, MVP): an optional modifier prefix
// "ctrl+", "alt+", "shift+" — in that order, only the modifiers present —
// followed by either a lowercased rune ("c", "?") or a named key ("enter",
// "tab", "backspace", "delete", "esc", "up", "down", "left", "right",
// "home", "end", "pgup", "pgdn"). Examples: "ctrl+c", "alt+enter",
// "shift+tab", "ctrl+j", "?".
//
// One special composite form exists: two space-separated chords, e.g.
// "g g", naming a double-press pair. It is recognized only by
// Registry.Register and Registry.Resolve as a double-press binding — there
// is no general chord-sequence framework, and ChordOf never produces this
// form itself (it names one keystroke).
type Chord string

// namedKeys maps every non-rune input.Key to its canonical chord name.
// KeyRune is intentionally absent: rune keys are named from their rune,
// not from this table.
var namedKeys = map[input.Key]string{
	input.KeyEnter:     "enter",
	input.KeyTab:       "tab",
	input.KeyBackspace: "backspace",
	input.KeyDelete:    "delete",
	input.KeyEscape:    "esc",
	input.KeyUp:        "up",
	input.KeyDown:      "down",
	input.KeyLeft:      "left",
	input.KeyRight:     "right",
	input.KeyHome:      "home",
	input.KeyEnd:       "end",
	input.KeyPgUp:      "pgup",
	input.KeyPgDn:      "pgdn",
}

// ChordOf returns the canonical Chord for one decoded keystroke. Modifiers
// are emitted in the fixed order ctrl, alt, shift, only for the ones k
// actually carries; a rune key is named by its lowercased rune (this is
// why Ctrl+A and Ctrl+a produce the same chord — consistent with
// input.KeyEvent's own doc, which says Ctrl+letter always arrives already
// lowercased); a named key uses namedKeys. Plain shifted runes (k.Shift ==
// false per input's doc — terminals rarely report Shift unambiguously for
// them) are not distinguished from their lowercase form at the chord
// level: "G" typed via Shift+g and "g" both canonicalize to Chord("g").
// Components that need a distinct binding for such a key must use a
// different chord (a modifier-qualified or named-key chord, both of which
// input can report unambiguously), never rely on rune case.
func ChordOf(k input.KeyEvent) Chord {
	var b strings.Builder
	if k.Ctrl {
		b.WriteString("ctrl+")
	}
	if k.Alt {
		b.WriteString("alt+")
	}
	if k.Shift {
		b.WriteString("shift+")
	}
	if name, ok := namedKeys[k.Key]; ok {
		b.WriteString(name)
	} else {
		b.WriteRune(unicode.ToLower(k.Rune))
	}
	return Chord(b.String())
}

// splitDouble reports whether c is the double-press composite form "x y"
// (exactly two space-separated, non-empty tokens) and returns its two
// parts. Any other shape — no space, empty tokens, more than one space —
// is not a double-press chord; the MVP supports no deeper chord sequence.
func splitDouble(c Chord) (seed, second Chord, ok bool) {
	s := string(c)
	i := strings.IndexByte(s, ' ')
	if i <= 0 || i == len(s)-1 {
		return "", "", false
	}
	rest := s[i+1:]
	if strings.IndexByte(rest, ' ') >= 0 {
		return "", "", false
	}
	return Chord(s[:i]), Chord(rest), true
}

// reservedOwners is the closed set of chords no other Owner may claim
// (blueprint 4.5, D3): app-shell's Ctrl+C policy (clear composer, then
// interrupt the in-flight turn, then quit on a fast second press) and
// help's "?" overlay toggle. Both stay live even while a modal scope is
// open — see Resolve.
var reservedOwners = map[Chord]string{
	"ctrl+c": "app-shell",
	"?":      "help",
}

func isReserved(c Chord) bool {
	_, ok := reservedOwners[c]
	return ok
}

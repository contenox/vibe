// Package keymap is beam's sole key arbiter. No component ever switches on
// a raw key: every binding is declared up front, a chord collision between
// simultaneously-reachable scopes fails at registration time (see
// Registry.Register), and a component learns "the user pressed X" only
// through the semantic Action a resolved chord produces. The help overlay
// is generated entirely from registrations (Registry.Help).
package keymap

import (
	"strings"
	"unicode"

	"github.com/contenox/contenox/internal/surfaces/beamtui/input"
)

// Scope names one keybinding context: the global catch-all, a focusable
// pane, or a modal. It is an open string type so later components can
// declare their own pane scopes, but ScopeGlobal and the modal set
// recognized by IsModal are load-bearing to Register and Resolve's
// collision/reachability rules. Any other value behaves as a generic pane
// scope: reachable with ScopeGlobal, never with another pane scope (only
// one pane is focused at a time — see FocusManager).
type Scope string

// Predeclared scopes. ScopeComposer and ScopeTranscript are ordinary panes.
// ScopePalette, ScopeApproval, ScopePicker and ScopeHelp are modal (see
// IsModal): while one is open it suspends the focused pane and ScopeGlobal
// except for the two reserved chords, which stay live under every modal.
const (
	ScopeGlobal     Scope = "global"
	ScopeComposer   Scope = "composer"
	ScopeTranscript Scope = "transcript"
	ScopePalette    Scope = "palette"
	ScopeApproval   Scope = "approval"
	ScopePicker     Scope = "picker"
	ScopeHelp       Scope = "help"
)

// modalScopes is the closed set of modal scopes: a modal is reachable only
// alone (plus the reserved chords passing through Global), a stronger
// isolation than an arbitrary pane scope gets, so the set cannot grow the
// way pane scopes can.
var modalScopes = map[Scope]bool{
	ScopePalette:  true,
	ScopeApproval: true,
	ScopePicker:   true,
	ScopeHelp:     true,
}

// IsModal reports whether s is one of the predeclared modal scopes.
func IsModal(s Scope) bool { return modalScopes[s] }

// reachable reports whether bindings in scopes a and b could ever be live
// at the same time — the same scope always collides with itself; Global
// collides with any pane; two different panes never collide; a modal
// collides only with itself. The reserved-chord passthrough is Resolve's
// runtime concern, enforced separately in Register.
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

// Chord is the canonical text form of one keystroke, produced by ChordOf or
// written as a literal by a Binding declaration: an optional modifier
// prefix "ctrl+"/"alt+"/"shift+" in that order, then a lowercased rune or a
// named key ("enter", "tab", "backspace", "delete", "esc", the arrows,
// "home", "end", "pgup", "pgdn"). Examples: "ctrl+c", "alt+enter", "?".
//
// One composite form exists: two space-separated chords ("g g"), a
// double-press pair recognized only by Registry.Register and
// Registry.Resolve. ChordOf never produces this form; it names one
// keystroke.
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
// carries; a rune key is named by its lowercased rune (Ctrl+A and Ctrl+a
// both canonicalize to the same chord, matching input.KeyEvent's own
// lowercasing), a named key uses namedKeys. A component needing a distinct
// binding for a shifted rune must use a modifier-qualified or named-key
// chord instead of relying on rune case.
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
// parts. No deeper chord sequence is supported.
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

// reservedOwners is the closed set of chords no other Owner may claim:
// app-shell's Ctrl+C policy and help's "?" overlay toggle. Both stay live
// even while a modal scope is open (see Resolve).
var reservedOwners = map[Chord]string{
	"ctrl+c": "app-shell",
	"?":      "help",
}

func isReserved(c Chord) bool {
	_, ok := reservedOwners[c]
	return ok
}

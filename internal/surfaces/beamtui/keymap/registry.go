package keymap

import (
	"fmt"
	"sort"
	"time"

	"github.com/contenox/beam/internal/surfaces/beamtui/input"
)

// doublePressWindow is how long a first "g"-style press waits for its
// second before the pair expires.
const doublePressWindow = 600 * time.Millisecond

// Binding declares one keybinding. Every field is required; Register
// rejects a Binding missing any of them, so no chord is ever unowned or
// undocumented.
type Binding struct {
	ID    string // stable identity
	Owner string // named in collision errors
	Keys  []Chord
	Scope Scope
	Help  string // the sole source of the help overlay's text for this binding
}

// Action is what a resolved chord becomes: routed to the focused
// component, never the raw key.
type Action struct {
	BindingID string
	Owner     string
	Chord     Chord
}

// HelpEntry is one row of the help overlay's content model, copied
// verbatim from a registered Binding: the overlay has no hardcoded strings.
type HelpEntry struct {
	Scope Scope
	Keys  []Chord
	Help  string
	Owner string
}

// pending is the double-press wait state: a first key of a registered
// double-press pair was seen and is waiting for its second within
// doublePressWindow.
type pending struct {
	seed Chord
	at   time.Time
	set  bool
}

// Registry is the single arbiter of every keybinding in beam: it holds
// every registration, resolves raw input.KeyEvents to semantic Actions
// (Resolve), and is the sole source of the help overlay's content (Help).
// Not safe for concurrent use; beam's input loop is single-threaded.
type Registry struct {
	// byScope[scope][chord] is the Binding that owns chord within scope;
	// chord may be a plain chord or a double-press composite ("g g").
	byScope map[Scope]map[Chord]Binding
	// seeds[scope][seed] is the Binding whose double-press chord begins
	// with seed, so Resolve can recognize "wait for the second key" and
	// Register can reject an ambiguous plain binding on the same chord.
	seeds map[Scope]map[Chord]Binding
	all   []Binding

	wait pending
}

// NewRegistry returns an empty Registry.
func NewRegistry() *Registry {
	return &Registry{
		byScope: make(map[Scope]map[Chord]Binding),
		seeds:   make(map[Scope]map[Chord]Binding),
	}
}

// Register validates and adds b, returning a descriptive error naming both
// owners in a collision instead of registering it:
//
//   - a missing ID, Owner, Scope, Help, or empty/duplicate Keys;
//   - a binding not owned by the reserved owner claiming "ctrl+c" or "?",
//     which stay live under every modal and are never overridable;
//   - a chord collision with any binding whose scope is simultaneously
//     reachable with b.Scope (see reachable);
//   - a double-press chord ("g g") colliding with a plain binding on its
//     seed ("g") in a reachable scope, or vice versa.
func (r *Registry) Register(b Binding) error {
	if err := validate(b); err != nil {
		return err
	}
	seen := make(map[Chord]bool, len(b.Keys))
	for _, c := range b.Keys {
		if seen[c] {
			return fmt.Errorf("keymap: binding %q (owner %q) declares chord %q more than once", b.ID, b.Owner, c)
		}
		seen[c] = true
		if owner, ok := reservedOwners[c]; ok && owner != b.Owner {
			return fmt.Errorf("keymap: chord %q is reserved for owner %q; binding %q (owner %q) may not claim it", c, owner, b.ID, b.Owner)
		}
	}
	for _, c := range b.Keys {
		if seed, _, ok := splitDouble(c); ok {
			if other, scope, found := r.findChord(b.Scope, seed); found {
				return fmt.Errorf("keymap: chord %q (binding %q, owner %q, scope %q) is ambiguous with %q (owner %q, scope %q) on prefix %q", c, b.ID, b.Owner, b.Scope, other.ID, other.Owner, scope, seed)
			}
			if other, scope, found := r.findChord(b.Scope, c); found {
				return fmt.Errorf("keymap: chord %q (binding %q, owner %q, scope %q) collides with %q (owner %q, scope %q)", c, b.ID, b.Owner, b.Scope, other.ID, other.Owner, scope)
			}
		} else {
			if other, scope, found := r.findChord(b.Scope, c); found {
				return fmt.Errorf("keymap: chord %q (binding %q, owner %q, scope %q) collides with %q (owner %q, scope %q)", c, b.ID, b.Owner, b.Scope, other.ID, other.Owner, scope)
			}
			if other, scope, found := r.findSeed(b.Scope, c); found {
				return fmt.Errorf("keymap: chord %q (binding %q, owner %q, scope %q) is ambiguous with double-press binding %q (owner %q, scope %q)", c, b.ID, b.Owner, b.Scope, other.ID, other.Owner, scope)
			}
		}
	}

	for _, c := range b.Keys {
		if seed, _, ok := splitDouble(c); ok {
			r.seedMap(b.Scope)[seed] = b
		}
		r.chordMap(b.Scope)[c] = b
	}
	r.all = append(r.all, b)
	return nil
}

// MustRegister calls Register and panics on error: a collision is a
// programming error meant to fail loudly, never handled at runtime.
func (r *Registry) MustRegister(b Binding) {
	if err := r.Register(b); err != nil {
		panic(err)
	}
}

func validate(b Binding) error {
	if b.ID == "" {
		return fmt.Errorf("keymap: binding missing ID (owner %q)", b.Owner)
	}
	if b.Owner == "" {
		return fmt.Errorf("keymap: binding %q missing Owner", b.ID)
	}
	if b.Scope == "" {
		return fmt.Errorf("keymap: binding %q (owner %q) missing Scope", b.ID, b.Owner)
	}
	if b.Help == "" {
		return fmt.Errorf("keymap: binding %q (owner %q) missing Help", b.ID, b.Owner)
	}
	if len(b.Keys) == 0 {
		return fmt.Errorf("keymap: binding %q (owner %q) has no Keys", b.ID, b.Owner)
	}
	for _, c := range b.Keys {
		if c == "" {
			return fmt.Errorf("keymap: binding %q (owner %q) has an empty chord", b.ID, b.Owner)
		}
	}
	return nil
}

func (r *Registry) chordMap(s Scope) map[Chord]Binding {
	m := r.byScope[s]
	if m == nil {
		m = make(map[Chord]Binding)
		r.byScope[s] = m
	}
	return m
}

func (r *Registry) seedMap(s Scope) map[Chord]Binding {
	m := r.seeds[s]
	if m == nil {
		m = make(map[Chord]Binding)
		r.seeds[s] = m
	}
	return m
}

// findChord returns a previously registered binding on chord in any scope
// reachable from scope (see reachable), for collision detection at
// Register time — a static fact, independent of any runtime modal state.
func (r *Registry) findChord(scope Scope, chord Chord) (Binding, Scope, bool) {
	for s2, m := range r.byScope {
		if !reachable(scope, s2) {
			continue
		}
		if b, ok := m[chord]; ok {
			return b, s2, true
		}
	}
	return Binding{}, "", false
}

// findSeed returns a previously registered double-press binding whose seed
// is chord, in any scope reachable from scope.
func (r *Registry) findSeed(scope Scope, chord Chord) (Binding, Scope, bool) {
	for s2, m := range r.seeds {
		if !reachable(scope, s2) {
			continue
		}
		if b, ok := m[chord]; ok {
			return b, s2, true
		}
	}
	return Binding{}, "", false
}

// Resolve turns one decoded keystroke into an Action, or reports false when
// it binds nothing. active names the scopes to check in priority order
// (FocusManager.ActiveScopes supplies it). Whenever active[0] is a modal
// scope, a non-reserved Global binding never resolves, but "ctrl+c" and "?"
// resolve from Global regardless.
//
// When the chord is the first half of a double-press binding reachable
// from active, Resolve returns false and remembers the pending seed. The
// next call either completes the pair within doublePressWindow and returns
// its Action, or resolves nothing and clears the pending state; an expired
// seed is discarded and the current keystroke evaluated fresh.
//
// now is the only clock Resolve uses.
func (r *Registry) Resolve(active []Scope, k input.KeyEvent, now time.Time) (Action, bool) {
	chord := ChordOf(k)
	modalTop := len(active) > 0 && IsModal(active[0])

	if r.wait.set {
		seed := r.wait.seed
		expired := now.Sub(r.wait.at) > doublePressWindow
		r.wait = pending{}
		if !expired {
			combined := Chord(string(seed) + " " + string(chord))
			if a, ok := r.lookup(active, combined, modalTop); ok {
				return a, true
			}
			return Action{}, false
		}
		// Expired: fall through and evaluate this keystroke fresh.
	}

	if r.hasSeed(active, chord, modalTop) {
		r.wait = pending{seed: chord, at: now, set: true}
		return Action{}, false
	}

	return r.lookup(active, chord, modalTop)
}

func (r *Registry) lookup(active []Scope, chord Chord, modalTop bool) (Action, bool) {
	reserved := isReserved(chord)
	for _, s := range active {
		if s == ScopeGlobal && modalTop && !reserved {
			continue
		}
		if m, ok := r.byScope[s]; ok {
			if b, ok := m[chord]; ok {
				return Action{BindingID: b.ID, Owner: b.Owner, Chord: chord}, true
			}
		}
	}
	return Action{}, false
}

func (r *Registry) hasSeed(active []Scope, chord Chord, modalTop bool) bool {
	for _, s := range active {
		if s == ScopeGlobal && modalTop {
			// No reserved chord is a double-press seed, so Global never
			// contributes a pending wait while a modal shadows it.
			continue
		}
		if m, ok := r.seeds[s]; ok {
			if _, ok := m[chord]; ok {
				return true
			}
		}
	}
	return false
}

// Help returns every registered Binding whose Scope is among active, sorted
// by Scope then Binding.ID so a golden test of the overlay never flakes on
// map order.
func (r *Registry) Help(active []Scope) []HelpEntry {
	want := make(map[Scope]bool, len(active))
	for _, s := range active {
		want[s] = true
	}
	sorted := append([]Binding(nil), r.all...)
	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i].Scope != sorted[j].Scope {
			return sorted[i].Scope < sorted[j].Scope
		}
		return sorted[i].ID < sorted[j].ID
	})
	out := make([]HelpEntry, 0, len(sorted))
	for _, b := range sorted {
		if !want[b.Scope] {
			continue
		}
		out = append(out, HelpEntry{
			Scope: b.Scope,
			Keys:  append([]Chord(nil), b.Keys...),
			Help:  b.Help,
			Owner: b.Owner,
		})
	}
	return out
}

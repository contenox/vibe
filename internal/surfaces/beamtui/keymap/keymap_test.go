package keymap

import (
	"strings"
	"testing"
	"time"

	"github.com/contenox/contenox/internal/surfaces/beamtui/input"
)

func TestUnit_ChordOfTable(t *testing.T) {
	cases := []struct {
		name string
		in   input.KeyEvent
		want Chord
	}{
		{"ctrl+c", input.KeyEvent{Key: input.KeyRune, Rune: 'c', Ctrl: true}, "ctrl+c"},
		{"alt+enter", input.KeyEvent{Key: input.KeyEnter, Alt: true}, "alt+enter"},
		{"shift+tab", input.KeyEvent{Key: input.KeyTab, Shift: true}, "shift+tab"},
		{"ctrl+j", input.KeyEvent{Key: input.KeyRune, Rune: 'j', Ctrl: true}, "ctrl+j"},
		{"plain question mark", input.KeyEvent{Key: input.KeyRune, Rune: '?'}, "?"},
		{"plain rune lowercases", input.KeyEvent{Key: input.KeyRune, Rune: 'A'}, "a"},
		{"plain lowercase unchanged", input.KeyEvent{Key: input.KeyRune, Rune: 'g'}, "g"},
		{"esc", input.KeyEvent{Key: input.KeyEscape}, "esc"},
		{"up", input.KeyEvent{Key: input.KeyUp}, "up"},
		{"down", input.KeyEvent{Key: input.KeyDown}, "down"},
		{"left", input.KeyEvent{Key: input.KeyLeft}, "left"},
		{"right", input.KeyEvent{Key: input.KeyRight}, "right"},
		{"home", input.KeyEvent{Key: input.KeyHome}, "home"},
		{"end", input.KeyEvent{Key: input.KeyEnd}, "end"},
		{"pgup", input.KeyEvent{Key: input.KeyPgUp}, "pgup"},
		{"pgdn", input.KeyEvent{Key: input.KeyPgDn}, "pgdn"},
		{"backspace", input.KeyEvent{Key: input.KeyBackspace}, "backspace"},
		{"delete", input.KeyEvent{Key: input.KeyDelete}, "delete"},
		{"ctrl+alt+shift ordering", input.KeyEvent{Key: input.KeyRune, Rune: 'x', Ctrl: true, Alt: true, Shift: true}, "ctrl+alt+shift+x"},
		{"ctrl+shift no alt", input.KeyEvent{Key: input.KeyRune, Rune: 'u', Ctrl: true, Shift: true}, "ctrl+shift+u"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ChordOf(tc.in); got != tc.want {
				t.Fatalf("ChordOf(%+v) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func mustReg(t *testing.T, r *Registry, b Binding) {
	t.Helper()
	if err := r.Register(b); err != nil {
		t.Fatalf("Register(%+v) unexpected error: %v", b, err)
	}
}

func wantErr(t *testing.T, r *Registry, b Binding, contains string) {
	t.Helper()
	err := r.Register(b)
	if err == nil {
		t.Fatalf("Register(%+v) = nil error, want one containing %q", b, contains)
	}
	if contains != "" && !strings.Contains(err.Error(), contains) {
		t.Fatalf("Register error = %q, want it to contain %q", err.Error(), contains)
	}
}

func TestUnit_RegisterRequiredFields(t *testing.T) {
	base := Binding{ID: "x.id", Owner: "x", Keys: []Chord{"a"}, Scope: ScopeComposer, Help: "does a thing"}

	missingID := base
	missingID.ID = ""
	wantErr(t, NewRegistry(), missingID, "ID")

	missingOwner := base
	missingOwner.Owner = ""
	wantErr(t, NewRegistry(), missingOwner, "Owner")

	missingScope := base
	missingScope.Scope = ""
	wantErr(t, NewRegistry(), missingScope, "Scope")

	missingHelp := base
	missingHelp.Help = ""
	wantErr(t, NewRegistry(), missingHelp, "Help")

	missingKeys := base
	missingKeys.Keys = nil
	wantErr(t, NewRegistry(), missingKeys, "Keys")

	emptyChord := base
	emptyChord.Keys = []Chord{""}
	wantErr(t, NewRegistry(), emptyChord, "")

	dup := base
	dup.Keys = []Chord{"a", "a"}
	wantErr(t, NewRegistry(), dup, "more than once")

	r := NewRegistry()
	mustReg(t, r, base)
}

func TestUnit_MustRegisterPanicsOnError(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("MustRegister did not panic on an invalid binding")
		}
	}()
	NewRegistry().MustRegister(Binding{})
}

func TestUnit_CollisionSameScope(t *testing.T) {
	r := NewRegistry()
	mustReg(t, r, Binding{ID: "a", Owner: "compA", Keys: []Chord{"ctrl+k"}, Scope: ScopeComposer, Help: "a"})
	wantErr(t, r, Binding{ID: "b", Owner: "compB", Keys: []Chord{"ctrl+k"}, Scope: ScopeComposer, Help: "b"}, "collides")
}

func TestUnit_CollisionGlobalVsPane(t *testing.T) {
	r := NewRegistry()
	mustReg(t, r, Binding{ID: "quit", Owner: "app-shell", Keys: []Chord{"ctrl+q"}, Scope: ScopeGlobal, Help: "quit"})
	wantErr(t, r, Binding{ID: "composer.foo", Owner: "composer", Keys: []Chord{"ctrl+q"}, Scope: ScopeComposer, Help: "foo"}, "collides")

	// And the reverse order: pane first, then global.
	r2 := NewRegistry()
	mustReg(t, r2, Binding{ID: "composer.foo", Owner: "composer", Keys: []Chord{"ctrl+q"}, Scope: ScopeComposer, Help: "foo"})
	wantErr(t, r2, Binding{ID: "quit", Owner: "app-shell", Keys: []Chord{"ctrl+q"}, Scope: ScopeGlobal, Help: "quit"}, "collides")
}

func TestUnit_NoCollisionBetweenDifferentPaneScopes(t *testing.T) {
	r := NewRegistry()
	mustReg(t, r, Binding{ID: "composer.up", Owner: "composer", Keys: []Chord{"up"}, Scope: ScopeComposer, Help: "history up"})
	mustReg(t, r, Binding{ID: "transcript.up", Owner: "transcript", Keys: []Chord{"up"}, Scope: ScopeTranscript, Help: "scroll up"})
}

func TestUnit_ModalIsolation(t *testing.T) {
	r := NewRegistry()
	// Same chord in two different modal scopes: never simultaneously
	// reachable (a modal is reachable only alone), so no collision.
	mustReg(t, r, Binding{ID: "palette.confirm", Owner: "palette", Keys: []Chord{"enter"}, Scope: ScopePalette, Help: "run"})
	mustReg(t, r, Binding{ID: "approval.confirm", Owner: "approval", Keys: []Chord{"enter"}, Scope: ScopeApproval, Help: "approve"})

	// Same chord within the same modal scope does collide.
	wantErr(t, r, Binding{ID: "palette.other", Owner: "palette2", Keys: []Chord{"enter"}, Scope: ScopePalette, Help: "x"}, "collides")

	// A modal chord never collides with a pane or global binding of a
	// different, non-reserved chord.
	mustReg(t, r, Binding{ID: "composer.enter", Owner: "composer", Keys: []Chord{"enter"}, Scope: ScopeComposer, Help: "submit"})
}

func TestUnit_ReservedChordProtectionBothDirections(t *testing.T) {
	// Direction one: the reserved owner registers first, a stranger's
	// later attempt on the same chord is rejected regardless of scope.
	r := NewRegistry()
	mustReg(t, r, Binding{ID: "app-shell.ctrlc", Owner: "app-shell", Keys: []Chord{"ctrl+c"}, Scope: ScopeGlobal, Help: "interrupt/quit"})
	wantErr(t, r, Binding{ID: "shell.sigint", Owner: "shell-pane", Keys: []Chord{"ctrl+c"}, Scope: ScopeTranscript, Help: "sigint"}, "reserved")
	wantErr(t, r, Binding{ID: "modal.ctrlc", Owner: "palette", Keys: []Chord{"ctrl+c"}, Scope: ScopePalette, Help: "x"}, "reserved")

	// Direction two: the stranger tries first — still rejected, order
	// does not matter.
	r2 := NewRegistry()
	wantErr(t, r2, Binding{ID: "shell.sigint", Owner: "shell-pane", Keys: []Chord{"ctrl+c"}, Scope: ScopeTranscript, Help: "sigint"}, "reserved")

	// "?" is reserved for "help" the same way.
	r3 := NewRegistry()
	mustReg(t, r3, Binding{ID: "help.toggle", Owner: "help", Keys: []Chord{"?"}, Scope: ScopeGlobal, Help: "help"})
	wantErr(t, r3, Binding{ID: "composer.question", Owner: "composer", Keys: []Chord{"?"}, Scope: ScopeComposer, Help: "x"}, "reserved")
}

func TestUnit_DoublePressCollidesWithPlainSeed(t *testing.T) {
	r := NewRegistry()
	mustReg(t, r, Binding{ID: "nav.top", Owner: "transcript", Keys: []Chord{"g g"}, Scope: ScopeTranscript, Help: "top"})
	wantErr(t, r, Binding{ID: "nav.g", Owner: "transcript", Keys: []Chord{"g"}, Scope: ScopeTranscript, Help: "x"}, "ambiguous")

	r2 := NewRegistry()
	mustReg(t, r2, Binding{ID: "nav.g", Owner: "transcript", Keys: []Chord{"g"}, Scope: ScopeTranscript, Help: "x"})
	wantErr(t, r2, Binding{ID: "nav.top", Owner: "transcript", Keys: []Chord{"g g"}, Scope: ScopeTranscript, Help: "top"}, "ambiguous")
}

func TestUnit_DoublePressWindow(t *testing.T) {
	active := []Scope{ScopeTranscript, ScopeGlobal}
	base := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	gEvent := input.KeyEvent{Key: input.KeyRune, Rune: 'g'}

	newReg := func(t *testing.T) *Registry {
		t.Helper()
		r := NewRegistry()
		mustReg(t, r, Binding{ID: "nav.top", Owner: "transcript", Keys: []Chord{"g g"}, Scope: ScopeTranscript, Help: "top"})
		return r
	}

	t.Run("pass within window", func(t *testing.T) {
		r := newReg(t)
		_, ok := r.Resolve(active, gEvent, base)
		if ok {
			t.Fatal("first g resolved immediately, want pending (false)")
		}
		a, ok := r.Resolve(active, gEvent, base.Add(300*time.Millisecond))
		if !ok || a.BindingID != "nav.top" {
			t.Fatalf("second g within window = %+v, %v, want nav.top action", a, ok)
		}
	})

	t.Run("expire", func(t *testing.T) {
		r := newReg(t)
		_, ok := r.Resolve(active, gEvent, base)
		if ok {
			t.Fatal("first g resolved immediately, want pending (false)")
		}
		_, ok = r.Resolve(active, gEvent, base.Add(700*time.Millisecond))
		if ok {
			t.Fatal("second g after the 600ms window resolved, want expiry (false)")
		}
	})

	t.Run("interruption clears pending and resolves nothing", func(t *testing.T) {
		r := newReg(t)
		mustReg(t, r, Binding{ID: "nav.h", Owner: "transcript", Keys: []Chord{"h"}, Scope: ScopeTranscript, Help: "left"})
		_, ok := r.Resolve(active, gEvent, base)
		if ok {
			t.Fatal("first g resolved immediately, want pending (false)")
		}
		hEvent := input.KeyEvent{Key: input.KeyRune, Rune: 'h'}
		a, ok := r.Resolve(active, hEvent, base.Add(100*time.Millisecond))
		if ok {
			t.Fatalf("interrupting key resolved %+v, want nothing", a)
		}
		// Pending is cleared: a fresh, correctly-timed "g g" now works again.
		_, ok = r.Resolve(active, gEvent, base.Add(200*time.Millisecond))
		if ok {
			t.Fatal("first g after interruption resolved immediately, want pending (false)")
		}
		a, ok = r.Resolve(active, gEvent, base.Add(400*time.Millisecond))
		if !ok || a.BindingID != "nav.top" {
			t.Fatalf("g g after interruption = %+v, %v, want nav.top action", a, ok)
		}
	})
}

func TestUnit_ResolvePrecedenceModalShadowsExceptReserved(t *testing.T) {
	r := NewRegistry()
	mustReg(t, r, Binding{ID: "app-shell.ctrlc", Owner: "app-shell", Keys: []Chord{"ctrl+c"}, Scope: ScopeGlobal, Help: "interrupt/quit"})
	mustReg(t, r, Binding{ID: "help.toggle", Owner: "help", Keys: []Chord{"?"}, Scope: ScopeGlobal, Help: "help"})
	mustReg(t, r, Binding{ID: "global.other", Owner: "app-shell", Keys: []Chord{"ctrl+p"}, Scope: ScopeGlobal, Help: "other"})
	mustReg(t, r, Binding{ID: "composer.enter", Owner: "composer", Keys: []Chord{"enter"}, Scope: ScopeComposer, Help: "submit"})
	mustReg(t, r, Binding{ID: "palette.enter", Owner: "palette", Keys: []Chord{"enter"}, Scope: ScopePalette, Help: "run"})

	now := time.Now()

	// No modal: pane binding and non-reserved global binding both resolve.
	noModal := []Scope{ScopeComposer, ScopeGlobal}
	if a, ok := r.Resolve(noModal, input.KeyEvent{Key: input.KeyEnter}, now); !ok || a.BindingID != "composer.enter" {
		t.Fatalf("composer enter = %+v, %v", a, ok)
	}
	if a, ok := r.Resolve(noModal, input.KeyEvent{Key: input.KeyRune, Rune: 'p', Ctrl: true}, now); !ok || a.BindingID != "global.other" {
		t.Fatalf("global ctrl+p with no modal = %+v, %v", a, ok)
	}

	// Modal open: the modal's own scope resolves normally.
	withModal := []Scope{ScopePalette, ScopeGlobal}
	if a, ok := r.Resolve(withModal, input.KeyEvent{Key: input.KeyEnter}, now); !ok || a.BindingID != "palette.enter" {
		t.Fatalf("palette enter under modal = %+v, %v", a, ok)
	}

	// Modal open: the non-reserved global binding is shadowed.
	if a, ok := r.Resolve(withModal, input.KeyEvent{Key: input.KeyRune, Rune: 'p', Ctrl: true}, now); ok {
		t.Fatalf("global ctrl+p under modal resolved %+v, want shadowed", a)
	}

	// Modal open: reserved chords still resolve from Global.
	if a, ok := r.Resolve(withModal, input.KeyEvent{Key: input.KeyRune, Rune: 'c', Ctrl: true}, now); !ok || a.BindingID != "app-shell.ctrlc" {
		t.Fatalf("ctrl+c under modal = %+v, %v, want app-shell.ctrlc", a, ok)
	}
	if a, ok := r.Resolve(withModal, input.KeyEvent{Key: input.KeyRune, Rune: '?'}, now); !ok || a.BindingID != "help.toggle" {
		t.Fatalf("? under modal = %+v, %v, want help.toggle", a, ok)
	}

	// An unbound chord anywhere resolves to nothing.
	if a, ok := r.Resolve(withModal, input.KeyEvent{Key: input.KeyRune, Rune: 'z'}, now); ok {
		t.Fatalf("unbound chord resolved %+v, want nothing", a)
	}
}

func TestUnit_CycleClosedLoop(t *testing.T) {
	f := NewFocusManager()
	f.SetOrder([]Scope{ScopeComposer, ScopeTranscript, ScopeHelp})

	if got := f.Focused(); got != ScopeComposer {
		t.Fatalf("initial focus = %q, want %q", got, ScopeComposer)
	}

	// Forward around the whole loop returns to start.
	for range 3 {
		f.Cycle(1)
	}
	if got := f.Focused(); got != ScopeComposer {
		t.Fatalf("after 3 forward cycles = %q, want %q (closed loop)", got, ScopeComposer)
	}

	// Backward wraps to the last element.
	f.Cycle(-1)
	if got := f.Focused(); got != ScopeHelp {
		t.Fatalf("cycle(-1) from start = %q, want %q", got, ScopeHelp)
	}

	// Forward from the wrapped position returns to start.
	f.Cycle(1)
	if got := f.Focused(); got != ScopeComposer {
		t.Fatalf("cycle(1) after wrap = %q, want %q", got, ScopeComposer)
	}

	// Empty order is a safe no-op.
	empty := NewFocusManager()
	empty.Cycle(1)
	if got := empty.Focused(); got != "" {
		t.Fatalf("empty FocusManager.Focused() = %q, want empty", got)
	}
}

func TestUnit_FocusManagerModalStack(t *testing.T) {
	f := NewFocusManager()
	f.SetOrder([]Scope{ScopeComposer, ScopeTranscript})
	f.Cycle(1) // focus Transcript

	if got := f.ActiveScopes(); len(got) != 2 || got[0] != ScopeTranscript || got[1] != ScopeGlobal {
		t.Fatalf("ActiveScopes with no modal = %v, want [transcript global]", got)
	}

	f.PushModal(ScopePalette)
	if got := f.ActiveScopes(); len(got) != 2 || got[0] != ScopePalette || got[1] != ScopeGlobal {
		t.Fatalf("ActiveScopes with one modal = %v, want [palette global]", got)
	}

	f.PushModal(ScopeHelp)
	if got := f.ActiveScopes(); got[0] != ScopeHelp {
		t.Fatalf("ActiveScopes with stacked modal = %v, want top = help", got)
	}

	top, ok := f.PopModal()
	if !ok || top != ScopeHelp {
		t.Fatalf("PopModal = %q, %v, want help, true", top, ok)
	}
	if got := f.ActiveScopes(); got[0] != ScopePalette {
		t.Fatalf("ActiveScopes after pop = %v, want top = palette", got)
	}

	top, ok = f.PopModal()
	if !ok || top != ScopePalette {
		t.Fatalf("PopModal = %q, %v, want palette, true", top, ok)
	}
	if got := f.ActiveScopes(); got[0] != ScopeTranscript {
		t.Fatalf("ActiveScopes after both pops = %v, want top = transcript (focused pane)", got)
	}

	if _, ok := f.PopModal(); ok {
		t.Fatal("PopModal on an empty stack reported ok=true")
	}
}

func TestUnit_HelpCompleteness(t *testing.T) {
	r := NewRegistry()
	mustReg(t, r, Binding{ID: "app-shell.ctrlc", Owner: "app-shell", Keys: []Chord{"ctrl+c"}, Scope: ScopeGlobal, Help: "interrupt or quit"})
	mustReg(t, r, Binding{ID: "help.toggle", Owner: "help", Keys: []Chord{"?"}, Scope: ScopeGlobal, Help: "toggle help"})
	mustReg(t, r, Binding{ID: "composer.submit", Owner: "composer", Keys: []Chord{"enter"}, Scope: ScopeComposer, Help: "submit"})
	mustReg(t, r, Binding{ID: "transcript.top", Owner: "transcript", Keys: []Chord{"g g"}, Scope: ScopeTranscript, Help: "scroll to top"})
	mustReg(t, r, Binding{ID: "palette.run", Owner: "palette", Keys: []Chord{"enter"}, Scope: ScopePalette, Help: "run command"})

	active := []Scope{ScopeGlobal, ScopeComposer, ScopeTranscript, ScopePalette}
	entries := r.Help(active)
	if len(entries) != len(r.all) {
		t.Fatalf("Help returned %d entries, want %d (every registered binding exactly once)", len(entries), len(r.all))
	}

	seen := make(map[string]int)
	for _, e := range entries {
		seen[e.Owner+"|"+e.Help]++
	}
	for _, b := range r.all {
		key := b.Owner + "|" + b.Help
		if seen[key] != 1 {
			t.Fatalf("binding %q (owner %q) appears %d times in Help, want exactly 1", b.ID, b.Owner, seen[key])
		}
	}

	// Stable order: scope, then ID.
	for i := 1; i < len(entries); i++ {
		if entries[i-1].Scope > entries[i].Scope {
			t.Fatalf("Help not sorted by Scope: %q before %q", entries[i-1].Scope, entries[i].Scope)
		}
	}

	// Restricting active drops bindings outside it.
	onlyGlobal := r.Help([]Scope{ScopeGlobal})
	for _, e := range onlyGlobal {
		if e.Scope != ScopeGlobal {
			t.Fatalf("Help([global]) returned scope %q", e.Scope)
		}
	}
	if len(onlyGlobal) != 2 {
		t.Fatalf("Help([global]) returned %d entries, want 2", len(onlyGlobal))
	}
}

func TestUnit_ResolveEmptyActiveIsSafe(t *testing.T) {
	r := NewRegistry()
	mustReg(t, r, Binding{ID: "a", Owner: "x", Keys: []Chord{"a"}, Scope: ScopeGlobal, Help: "a"})
	if a, ok := r.Resolve(nil, input.KeyEvent{Key: input.KeyRune, Rune: 'a'}, time.Now()); ok {
		t.Fatalf("Resolve with nil active resolved %+v, want nothing", a)
	}
}

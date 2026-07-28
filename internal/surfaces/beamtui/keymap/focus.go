package keymap

// FocusManager owns which single pane scope currently has focus and the
// stack of open modal scopes. It is pure bookkeeping: it never decides
// whether a chord resolves (that is Registry.Resolve, acting on the scopes
// ActiveScopes reports).
type FocusManager struct {
	order   []Scope
	focused int
	stack   []Scope
}

// NewFocusManager returns a FocusManager with no focus order and no open
// modal.
func NewFocusManager() *FocusManager { return &FocusManager{} }

// SetOrder declares the closed cycle of focusable pane scopes Cycle walks,
// and resets focus to order's first element. Meant to be called once at
// init, not per-frame.
func (f *FocusManager) SetOrder(order []Scope) {
	f.order = append([]Scope(nil), order...)
	f.focused = 0
}

// Focused returns the currently focused pane scope, or "" if SetOrder was
// never called with a non-empty order.
func (f *FocusManager) Focused() Scope {
	if len(f.order) == 0 {
		return ""
	}
	return f.order[f.focused]
}

// Cycle moves focus by delta (+1 forward, -1 backward) as a closed loop
// over the order SetOrder declared, wrapping past either end. A nil/empty
// order makes Cycle a no-op.
func (f *FocusManager) Cycle(delta int) {
	n := len(f.order)
	if n == 0 {
		return
	}
	f.focused = ((f.focused+delta)%n + n) % n
}

// PushModal opens a modal scope on top of the modal stack, suspending the
// focused pane. Global bindings are suspended too except the two reserved
// chords; that shadowing is Resolve's job, not FocusManager's.
func (f *FocusManager) PushModal(s Scope) {
	f.stack = append(f.stack, s)
}

// PopModal closes the topmost open modal scope and returns it; ok is false
// and the returned Scope is "" if no modal was open.
func (f *FocusManager) PopModal() (Scope, bool) {
	if len(f.stack) == 0 {
		return "", false
	}
	top := f.stack[len(f.stack)-1]
	f.stack = f.stack[:len(f.stack)-1]
	return top, true
}

// ActiveScopes returns the scopes Registry.Resolve should check, topmost
// priority first: the topmost modal scope plus ScopeGlobal when a modal is
// open (the focused pane is fully suspended), otherwise the focused pane
// plus ScopeGlobal. Resolve infers the reserved-chords-only restriction on
// Global from active[0] being a modal scope, so the order here is load-bearing.
func (f *FocusManager) ActiveScopes() []Scope {
	if len(f.stack) > 0 {
		return []Scope{f.stack[len(f.stack)-1], ScopeGlobal}
	}
	return []Scope{f.Focused(), ScopeGlobal}
}

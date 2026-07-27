package keymap

// FocusManager owns which single pane scope currently has focus and the
// stack of open modal scopes; it is the focus/navigation half of the
// keymap-registry component (blueprint 4.5). It is pure bookkeeping — it
// never itself decides whether a chord resolves, that is Registry.Resolve
// acting on the scopes ActiveScopes reports; FocusManager only tracks
// "which scopes" so app-shell doesn't have to.
type FocusManager struct {
	order   []Scope
	focused int
	stack   []Scope
}

// NewFocusManager returns a FocusManager with no focus order and no open
// modal.
func NewFocusManager() *FocusManager { return &FocusManager{} }

// SetOrder declares the closed cycle of focusable pane scopes Cycle walks,
// and resets focus to order's first element. Calling it again re-derives
// the cycle (and focus) from scratch — components register their panes at
// init, this is not meant to be called per-frame.
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

// Cycle moves focus by delta — +1 forward, -1 backward — as a closed loop
// over the order SetOrder declared: cycling past either end wraps to the
// other. Any other delta magnitude walks that many steps in one call. A
// nil/empty order makes Cycle a no-op.
func (f *FocusManager) Cycle(delta int) {
	n := len(f.order)
	if n == 0 {
		return
	}
	f.focused = ((f.focused+delta)%n + n) % n
}

// PushModal opens a modal scope on top of the modal stack, suspending the
// focused pane. Global bindings are suspended too except the two reserved
// chords — that shadowing is Resolve's runtime job, driven by
// ActiveScopes's output, not something FocusManager enforces itself.
func (f *FocusManager) PushModal(s Scope) {
	f.stack = append(f.stack, s)
}

// PopModal closes the topmost open modal scope and returns it; ok is false
// and the returned Scope is "" if no modal was open. Escape's fixed
// priority stack (blueprint 4.5: "Esc closes exactly the topmost modal per
// press") is exactly one PopModal call per press.
func (f *FocusManager) PopModal() (Scope, bool) {
	if len(f.stack) == 0 {
		return "", false
	}
	top := f.stack[len(f.stack)-1]
	f.stack = f.stack[:len(f.stack)-1]
	return top, true
}

// ActiveScopes returns the scopes Registry.Resolve should check, topmost
// priority first: when a modal is open, the topmost modal scope plus
// ScopeGlobal (the focused pane is fully suspended — not returned at all);
// otherwise the focused pane plus ScopeGlobal. Resolve infers the
// reserved-chords-only restriction on Global purely from active[0] being a
// modal scope, so returning exactly these two elements in this order is
// what makes that inference correct.
func (f *FocusManager) ActiveScopes() []Scope {
	if len(f.stack) > 0 {
		return []Scope{f.stack[len(f.stack)-1], ScopeGlobal}
	}
	return []Scope{f.Focused(), ScopeGlobal}
}

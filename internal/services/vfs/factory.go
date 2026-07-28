package vfs

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync/atomic"
)

// Factory holds the serve-level allowlist of workspace roots a browser
// client may choose as a session's workspace. A directory that is a root, or
// lies under one, is permitted (see Resolve for the containment rule); the
// first root is the default, used for the sentinel "/" or an empty request.
// The root set is swapped atomically (see current): every reader sees either
// the old set or the new one, never a torn one, without locking.
type Factory struct {
	// current is the live root snapshot, swapped wholesale by SetRoots.
	current atomic.Pointer[rootSet]
}

// rootSet is an immutable snapshot of the allowlist: built once, never
// mutated, only replaced.
type rootSet struct {
	// roots is the ordered, de-duplicated, symlink-resolved allowlist; roots[0] is the default.
	roots []string
	// display maps a resolved root back to the operator-configured path, for human-facing labels.
	display map[string]string
}

// buildRootSet cleans, absolutizes, symlink-resolves, and de-duplicates
// roots into an immutable snapshot. At least one root is required.
func buildRootSet(roots ...string) (*rootSet, error) {
	rs := &rootSet{display: map[string]string{}}
	seen := map[string]struct{}{}
	for _, r := range roots {
		if r == "" {
			continue
		}
		resolved, err := ResolveRoot(r)
		if err != nil {
			return nil, fmt.Errorf("vfs: workspace root %q: %w", r, err)
		}
		if _, dup := seen[resolved]; dup {
			continue
		}
		seen[resolved] = struct{}{}
		rs.roots = append(rs.roots, resolved)
		if _, ok := rs.display[resolved]; !ok {
			rs.display[resolved] = filepath.Clean(r)
		}
	}
	if len(rs.roots) == 0 {
		return nil, fmt.Errorf("vfs: at least one workspace root is required")
	}
	return rs, nil
}

// defaultRoot returns the snapshot's default root (roots[0]).
func (rs *rootSet) defaultRoot() string {
	if len(rs.roots) == 0 {
		return ""
	}
	return rs.roots[0]
}

// describe joins the resolved roots for a teaching refusal message.
func (rs *rootSet) describe() string {
	return strings.Join(rs.roots, ", ")
}

// NewFactory builds a Factory from an ordered list of roots; the first is
// the default. At least one root is required.
func NewFactory(roots ...string) (*Factory, error) {
	rs, err := buildRootSet(roots...)
	if err != nil {
		return nil, err
	}
	f := &Factory{}
	f.current.Store(rs)
	return f, nil
}

// SetRoots atomically replaces the allowlist with a freshly-normalized
// snapshot (same rules as NewFactory). An in-flight reader continues on the
// old snapshot until the swap lands. At least one root is required; an
// invalid list is refused and the previous allowlist is left untouched.
// Default is always the new roots[0] — a caller that must keep a fixed
// default (serve) is responsible for keeping it first in the list it passes.
func (f *Factory) SetRoots(roots []string) error {
	rs, err := buildRootSet(roots...)
	if err != nil {
		return err
	}
	f.current.Store(rs)
	return nil
}

// load returns the live snapshot, falling back to an empty one for a
// zero-value Factory.
func (f *Factory) load() *rootSet {
	if rs := f.current.Load(); rs != nil {
		return rs
	}
	return &rootSet{display: map[string]string{}}
}

// Roots returns the allowlisted roots in configured order. The slice is a
// copy; callers may not mutate the Factory through it.
func (f *Factory) Roots() []string {
	rs := f.load()
	out := make([]string, len(rs.roots))
	copy(out, rs.roots)
	return out
}

// Default returns the default root (the first configured), used for the "/"
// / empty sentinel.
func (f *Factory) Default() string {
	return f.load().defaultRoot()
}

// DescribeRoots returns a human-facing, comma-separated list of the
// configured roots, for refusal messages.
func (f *Factory) DescribeRoots() string {
	return f.load().describe()
}

// Resolve maps a requested root to a permitted, resolved directory. "/" and
// "" resolve to the default root; any other value must be an allowlisted
// root or a segment-aware subpath of one, and is returned as the requested
// subpath, not its containing root. A path under the runtime's control plane
// is refused first with ErrControlPlane; anything else outside every root is
// refused with ErrNotAllowed.
func (f *Factory) Resolve(root string) (string, error) {
	rs := f.load()
	if root == "" || root == "/" {
		return rs.defaultRoot(), nil
	}
	resolved, err := ResolveRoot(root)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrNotAllowed, err)
	}
	if denied, ok := deniedResolved(resolved); ok {
		return "", controlPlaneError(root, denied)
	}
	for _, r := range rs.roots {
		if within(r, resolved) {
			return resolved, nil
		}
	}
	return "", fmt.Errorf("%w: %s is not under any configured workspace root; roots: %s", ErrNotAllowed, root, rs.describe())
}

// Allows reports whether root resolves to a permitted directory (the root itself
// or a contained subpath), returning the normalized directory when it does.
func (f *Factory) Allows(root string) (string, bool) {
	resolved, err := f.Resolve(root)
	if err != nil {
		return "", false
	}
	return resolved, true
}

// ErrCwdNotPermitted is the sentinel every session-cwd refusal wraps (see
// ResolveSessionCwd); callers translate it via errors.Is into their own
// transport error.
var ErrCwdNotPermitted = errors.New("session cwd is not permitted")

// cwdError carries a refusal message verbatim while matching
// ErrCwdNotPermitted under errors.Is.
type cwdError struct{ msg string }

func (e *cwdError) Error() string { return e.msg }
func (e *cwdError) Is(target error) bool {
	return target == ErrCwdNotPermitted
}

func cwdRefusal(format string, args ...any) error {
	return &cwdError{msg: fmt.Sprintf(format, args...)}
}

// ResolveSessionCwd is the one decision procedure for which directory a
// session runs in. Rules, in order: non-empty cwd must be absolute; "/" or
// empty means "unspecified" and resolves to f's default root (or fallback
// when f is nil); otherwise cwd must resolve under an allowlisted root, or
// (with f nil, the stdio path with no allowlist) is returned unchanged.
func ResolveSessionCwd(f *Factory, cwd, fallback string) (string, error) {
	if cwd != "" && !filepath.IsAbs(cwd) {
		return "", cwdRefusal("cwd must be an absolute path, got %q", cwd)
	}
	if f == nil {
		if cwd == "" {
			return fallback, nil
		}
		// Control plane is never a session cwd, even with no allowlist configured.
		if denied, ok := IsControlPlanePath(cwd); ok {
			return "", cwdRefusal("workspace directory %q is inside the runtime's control plane (%s), which is never a session workspace", cwd, denied)
		}
		return cwd, nil
	}
	resolved, err := f.Resolve(cwd)
	if err != nil {
		// Control-plane refusal is distinct from a plain outside-allowlist refusal.
		if errors.Is(err, ErrControlPlane) {
			return "", cwdRefusal("workspace directory %q is inside the runtime's control plane, which is never a session workspace — the runtime never runs a session where it could reach its own config, database, or policies", cwd)
		}
		return "", cwdRefusal("workspace directory %q is not under any configured workspace root; roots: %s", cwd, f.DescribeRoots())
	}
	return resolved, nil
}

// Open returns a View rooted at root, which must be permitted (via Resolve
// semantics, so "/" opens the default root and a contained subpath opens that
// subpath).
func (f *Factory) Open(root string) (*View, error) {
	resolved, err := f.Resolve(root)
	if err != nil {
		return nil, err
	}
	return newView(resolved)
}

// View is a root-bound convenience wrapper over Contain. It caches the
// symlink-resolved root so repeated Resolve calls avoid re-walking it.
type View struct {
	root     string
	realRoot string
	// privileged waives only the control-plane deny (escape containment
	// still holds). Set exclusively by OpenPrivilegedView.
	privileged bool
}

// OpenView returns a View rooted at root, resolving its symlinks. Unlike
// Factory.Open it enforces no allowlist — for a single fixed root trusted by
// construction.
func OpenView(root string) (*View, error) {
	resolved, err := ResolveRoot(root)
	if err != nil {
		return nil, err
	}
	return newView(resolved)
}

// OpenPrivilegedView returns a View whose resolutions skip the
// control-plane deny; escape containment is still enforced exactly as for
// any other view. Reserved for the runtime's own internal consumers reading
// its own governing state (chain-agent discovery, the chain-editor API) —
// never for an agent-facing surface (session cwd, /files browse, local_fs,
// search).
func OpenPrivilegedView(root string) (*View, error) {
	v, err := OpenView(root)
	if err != nil {
		return nil, err
	}
	v.privileged = true
	return v, nil
}

// newView builds a View for an already-resolved root.
func newView(resolved string) (*View, error) {
	realRoot, err := ResolveRoot(resolved)
	if err != nil {
		return nil, err
	}
	return &View{root: resolved, realRoot: realRoot}, nil
}

// Root returns the resolved root of this view.
func (v *View) Root() string { return v.root }

// Resolve contains candidate within the view's root (see Contain).
func (v *View) Resolve(candidate string) (string, error) {
	return containWithinOpts(v.realRoot, v.root, candidate, v.privileged)
}

// Contains reports whether an already-absolute path lies within the view root.
func (v *View) Contains(abs string) bool {
	absPath, err := filepath.Abs(abs)
	if err != nil {
		return false
	}
	return within(v.realRoot, filepath.Clean(absPath))
}

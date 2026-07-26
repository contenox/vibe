package vfs

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync/atomic"
)

// Factory holds the serve-level allowlist of workspace roots — the set of
// directories a browser client may choose as a session's workspace. Choosing a
// directory outside every configured root is refused; a directory that IS a
// root, or lies UNDER one, is permitted (see Resolve for the containment rule).
// The first root is the default, used when a client asks for the sentinel "/"
// (or nothing), which keeps existing clients that always send cwd:"/" working.
//
// # Containment, not exact match
//
// This Factory once required a requested root to EQUAL an allowlisted root
// byte-for-byte. That was false protection dressed as safety: it refused
// legitimate subdirectories of a granted root (you could grant ~/src but not
// browse ~/src/project) while adding nothing an escape check does not already
// give — the symlink/traversal guard in Contain/within is what actually keeps a
// path inside its root. The SCOPE of a workspace is the operator's choice:
// granting a root grants everything under it, and the runtime's job is to enforce
// that boundary honestly (a sibling, a prefix-trick
// neighbour like /home/userX against /home/user, and a symlink escaping all
// roots are all refused), not to second-guess the operator by also demanding an
// exact path.
//
// # Hot reload
//
// The root set is swapped ATOMICALLY (current, below): a grant added or removed
// at runtime replaces the whole snapshot in one store, so every reader either
// sees the old set or the new set, never a torn one, and no reader takes a lock.
// SetRoots is the writer; Roots/Default/Resolve/Allows/Open are the readers.
type Factory struct {
	// current is the live root snapshot, swapped wholesale by SetRoots. Readers
	// load it once and read from that immutable value, so the allowlist can change
	// under them without a lock and without a torn read.
	current atomic.Pointer[rootSet]
}

// rootSet is an immutable snapshot of the allowlist: once built it is never
// mutated, only replaced (see Factory.current). Sharing one is safe across
// goroutines precisely because nothing writes to it after construction.
type rootSet struct {
	// roots is the ordered, de-duplicated, symlink-resolved allowlist. roots[0]
	// is the default.
	roots []string
	// display maps a resolved root back to the path the operator configured, for
	// human-facing labels.
	display map[string]string
}

// buildRootSet cleans, absolutizes, symlink-resolves, and de-duplicates roots
// (preserving first-seen order), returning an immutable snapshot. At least one
// root is required. It is the shared core of NewFactory and SetRoots so the two
// entry points normalize identically.
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

// describe joins the resolved roots for a teaching refusal message, so a
// rejected caller is told exactly which directories it may choose from rather
// than being left to probe.
func (rs *rootSet) describe() string {
	return strings.Join(rs.roots, ", ")
}

// NewFactory builds a Factory from an ordered list of roots. The first root is
// the default. Roots are cleaned, made absolute, symlink-resolved, and
// de-duplicated (preserving first-seen order). At least one root is required.
func NewFactory(roots ...string) (*Factory, error) {
	rs, err := buildRootSet(roots...)
	if err != nil {
		return nil, err
	}
	f := &Factory{}
	f.current.Store(rs)
	return f, nil
}

// SetRoots atomically replaces the allowlist with a freshly-normalized snapshot
// built from roots (same cleaning/resolving/de-duplication as NewFactory). It is
// the hot-reload writer: serve calls it when a workspace-root grant is added or
// removed, and every in-flight reader continues on the old snapshot until the
// swap lands, then sees the new one — no lock, no torn read.
//
// At least one root is required, so an empty or all-empty list is refused and
// the previous allowlist is left untouched (a validation failure never leaves
// serve with no roots).
//
// # What happens to Default when the old default is removed
//
// The default is always the NEW roots[0]. If the caller drops the directory that
// used to be first and passes a different one first, that new first entry
// becomes the default. Serve never lets this happen by accident: it always
// prepends its launch-time base roots (its served project directory first) and
// only APPENDS the durable grants, so the served directory stays roots[0] across
// every reload and the default a client resolves "/" to never shifts under it.
// The primitive keeps the general rule (first-is-default); serve's construction
// keeps the guarantee (base is always first).
func (f *Factory) SetRoots(roots []string) error {
	rs, err := buildRootSet(roots...)
	if err != nil {
		return err
	}
	f.current.Store(rs)
	return nil
}

// load returns the live snapshot. A Factory built via NewFactory/SetRoots always
// has one; the empty fallback keeps a zero-value Factory (never constructed
// through those paths) from panicking a reader.
func (f *Factory) load() *rootSet {
	if rs := f.current.Load(); rs != nil {
		return rs
	}
	return &rootSet{display: map[string]string{}}
}

// Roots returns the allowlisted roots in configured order (resolved absolute
// paths). The slice is a copy; callers may not mutate the Factory through it.
func (f *Factory) Roots() []string {
	rs := f.load()
	out := make([]string, len(rs.roots))
	copy(out, rs.roots)
	return out
}

// Default returns the default root (the first configured), used for the "/" /
// empty sentinel.
func (f *Factory) Default() string {
	return f.load().defaultRoot()
}

// DescribeRoots returns a human-facing, comma-separated list of the configured
// roots, for teaching refusals ("… roots: /a, /b"). It reads the live snapshot,
// so a message names the roots as they stand at the moment of refusal.
func (f *Factory) DescribeRoots() string {
	return f.load().describe()
}

// Resolve maps a requested root to a permitted, resolved directory. The sentinel
// "/" and the empty string both resolve to the default root — the compat story
// for clients (beam today) that always send cwd:"/". Any other value is
// symlink/abs-normalized and permitted when it IS an allowlisted root or lies
// UNDER one; the resolved value returned is the requested directory itself (the
// contained subpath), NOT the containing root, so a client that asked to browse
// or run in a subdirectory gets that subdirectory. A value under no configured
// root — a sibling, a prefix-trick neighbour, or a symlink whose real target
// escapes every root — is refused with ErrNotAllowed and a message naming the
// roots.
//
// Segment-aware containment: the check is filepath.Rel-based (see within), so
// /home/userX is NOT "under" /home/user even though it shares the string prefix.
// Symlink safety: ResolveRoot follows symlinks before the check, so a link inside
// a granted root that points outside every root resolves to its real target and
// is refused.
//
// Control-plane carveout (vfs-invariant slice, 2026-07-21 — containment made it
// live): a path AT or UNDER the runtime's control plane (~/.contenox: config,
// database, HITL policies, chains, declared agents, models) is refused BEFORE the
// containment check, with its OWN teaching error (ErrControlPlane), distinct from
// the not-under-roots refusal. Checking first — rather than "after containment
// succeeds" — means a denied path is named as the control plane whether or not it
// also happens to sit under a granted root, which is the more honest message. The
// candidate is symlink-resolved first (ResolveRoot), so a link INTO the control
// plane resolves to its real target and is caught. See controlplane.go for the
// invariant and why it is process-global.
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
		// within(r, resolved) is true when resolved IS r or lies under it, and is
		// segment-aware (a sibling or prefix-trick neighbour is rejected). r is
		// already symlink-resolved (buildRootSet), and resolved is too, so this is
		// a pure path comparison with no further I/O.
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

// ErrCwdNotPermitted is the ONE sentinel every session-cwd refusal wraps (see
// ResolveSessionCwd). Callers translate it into their own transport error —
// apiframework.InvalidParameterValue for the REST surface,
// libacp.ErrInvalidParams for the ACP surface — via errors.Is, and take the
// user-facing text from Error(). The sentinel exists so the DECISION lives here
// while the WIRE SHAPE stays each transport's own concern.
var ErrCwdNotPermitted = errors.New("session cwd is not permitted")

// cwdError carries a refusal message verbatim while still matching
// ErrCwdNotPermitted under errors.Is. A plain fmt.Errorf("%w: ...") would splice
// the sentinel's text into the message every caller forwards to a user; this
// keeps the message exactly what the operator should read.
type cwdError struct{ msg string }

func (e *cwdError) Error() string { return e.msg }
func (e *cwdError) Is(target error) bool {
	return target == ErrCwdNotPermitted
}

func cwdRefusal(format string, args ...any) error {
	return &cwdError{msg: fmt.Sprintf(format, args...)}
}

// ResolveSessionCwd is the ONE decision procedure for "which concrete directory
// does this session run in". Every surface that opens or re-roots a session — the
// ACP session/new, session/load and session/resume paths, and the REST fleet
// dispatch — resolves through here so the workspace-root envelope cannot be
// enforced in one place and skipped in another. Its rules, in order:
//
//  1. A non-empty cwd MUST be absolute. A relative path is refused outright,
//     before any allowlist is consulted, because a relative cwd is meaningless
//     to a session that will run in some other process's working directory —
//     and, with no allowlist configured, would otherwise be adopted verbatim.
//     An empty cwd is not a path but the ABSENCE of one, and falls through to
//     rule 2.
//  2. The sentinel "/" and an empty cwd mean "unspecified". With an allowlist
//     configured they resolve to its default root (the compat story for clients
//     that always send cwd:"/"). With none configured, an empty cwd resolves to
//     fallback — the caller's own default root, "" for a caller that has none.
//  3. With an allowlist configured, any other value must resolve to an
//     allowlisted root OR a directory contained under one; otherwise the request
//     is refused. The concrete directory returned is the requested subpath, not
//     the containing root.
//  4. With NO allowlist configured (f nil — the stdio ACP path), an absolute cwd
//     is returned unchanged: there is no server-side envelope to enforce and the
//     editor owns the filesystem.
//
// The nil-allowlist default is deliberately the CALLER's (fallback) rather than a
// value invented here: "unspecified" means different things to a transport that
// has no notion of a project root (ACP stdio, which passes "" and leaves the cwd
// unspecified for the editor to interpret) and to one that does (fleet dispatch,
// which passes its configured project root). One procedure, one parameter, no
// second implementation.
func ResolveSessionCwd(f *Factory, cwd, fallback string) (string, error) {
	if cwd != "" && !filepath.IsAbs(cwd) {
		return "", cwdRefusal("cwd must be an absolute path, got %q", cwd)
	}
	if f == nil {
		if cwd == "" {
			return fallback, nil
		}
		// Even with no allowlist (the stdio/editor path, where the editor owns the
		// filesystem), the control plane is never a session cwd. When serve has
		// registered one this refuses it; on the stdio path none is registered, so
		// this is a no-op and the cwd passes through as before.
		if denied, ok := IsControlPlanePath(cwd); ok {
			return "", cwdRefusal("workspace directory %q is inside the runtime's control plane (%s), which is never a session workspace", cwd, denied)
		}
		return cwd, nil
	}
	resolved, err := f.Resolve(cwd)
	if err != nil {
		// The control-plane refusal is its own teaching case, distinct from a cwd
		// that is merely outside the allowlist.
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
	// privileged waives ONLY the control-plane deny for this view's resolutions
	// (escape containment always holds). Set exclusively by OpenPrivilegedView —
	// the runtime reading its own governing state. Never reachable from the
	// agent-facing constructors.
	privileged bool
}

// OpenView returns a View rooted at root, resolving its symlinks. Unlike
// Factory.Open it enforces no allowlist — use it for a single fixed root that is
// trusted by construction (e.g. a serve-owned browse root).
func OpenView(root string) (*View, error) {
	resolved, err := ResolveRoot(root)
	if err != nil {
		return nil, err
	}
	return newView(resolved)
}

// OpenPrivilegedView returns a View whose resolutions skip the control-plane
// deny — and NOTHING else (escape containment is enforced exactly as for every
// other view).
//
// The invariant this package enforces is "an AGENT must never fs-reach the
// runtime's governing state." The runtime reading its OWN state is not that
// threat — it is how the state exists at all: chain-agent discovery walks
// ~/.contenox for agent-*.json, serve's chain wiring and the operator's
// chain-editor API are deliberately rooted AT the control plane. Routing those
// through the guarded path bricked discovery at boot the day the carveout
// landed ("path is inside the runtime control plane" on the runtime's own
// directory) — this constructor is the sanctioned lane for exactly those
// internal consumers, and it must never be handed to an agent-facing surface
// (session cwd, /files browse, local_fs, search all stay on the guarded
// constructors).
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

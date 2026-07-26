package vfs

import (
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
)

// ErrControlPlane is returned (wrapped) when a path resolves AT or UNDER the
// runtime's control plane — the directories that hold the runtime's OWN
// governing state (its config, database, HITL policies, chains, declared agents,
// models). It is a DISTINCT refusal from ErrNotAllowed (a path outside every
// workspace root) and ErrEscape (a path leaving its root): those are boundary
// MISTAKES, this is a SECURITY invariant the model and the operator should
// understand, not a mystery 422. Callers translate it to their transport error
// via errors.Is and forward Error() as the teaching text.
//
// # The invariant (control-plane isolation)
//
// An agent must never fs-reach its own governing state. Could it read or write
// ~/.contenox, it could read the very HITL policy that gates it, forge the
// approval envelopes it must not, or corrupt the database that is the runtime's
// source of truth. So the control plane is NEVER a workspace, a session cwd, a
// browsable root, or a resolvable subpath — EVEN when it sits under a granted
// workspace root, and even reached via a symlink. This holds unconditionally:
// there is no envelope flag and no config to disable it.
//
// # Why this became URGENT (2026-07-21)
//
// Workspace roots gained CONTAINMENT semantics today (see Factory.Resolve):
// granting a root now grants everything under it. Before, only an exact
// allowlisted directory resolved, so ~/.contenox was reachable only if it was
// granted verbatim (nobody does that). With containment, a single broad grant
// like /home/user makes its child ~/.contenox reachable through every fs surface
// — the /files explorer, the local_fs agent tool, a session cwd, workspace
// search. And serve's own default root is the PARENT of its .contenox dir
// (workspaceRoot = filepath.Dir(contenoxDir)), so the control plane is a direct
// child of a granted root out of the box. Containment made the standing invariant
// LIVE; this carveout closes it.
//
// # Process-global, not per-Factory (deliberate)
//
// The control plane is ONE fixed fact for the whole serve process, but it must be
// honored by MANY independent vfs objects: the /files Factory, the search
// Factory, every local_fs View, every shellsession cwd resolution. Threading a
// denied set through each of them (and each constructor, several sibling-owned)
// would leave a gap the first missed wiring reopens. A single process-global
// denylist — set once at serve boot, consulted by the shared low-level guard
// (Factory.Resolve, ResolveSessionCwd, containWithin) — is the honest model: one
// control plane, refused everywhere, with no per-object opt-in to forget. It is
// NOT part of the mutable root set, so a SetRoots hot-reload never clears it. On
// the stdio/CLI paths that never call SetControlPlaneDenied the denylist is empty
// and nothing is denied, so behavior there is exactly as before.

// ErrControlPlane is the sentinel every control-plane refusal wraps.
var ErrControlPlane = errors.New("path is inside the runtime control plane")

// controlPlaneDenied is the process-global, symlink-resolved denylist. A nil
// pointer or empty slice means no control plane is registered — nothing is
// denied and every resolution behaves as it did before this carveout.
var controlPlaneDenied atomic.Pointer[[]string]

// SetControlPlaneDenied registers the runtime's control-plane directories. serve
// calls it ONCE at boot with its resolved ~/.contenox data/config dir(s); each
// path is absolutized and symlink-resolved so the stored form matches the
// resolved candidates it is compared against, and duplicates are collapsed.
// Calling it with no paths CLEARS the denylist — the reset a test uses between
// cases. It is safe to call before any request is served and is never called on
// a hot-reload, so the denylist survives every SetRoots.
func SetControlPlaneDenied(paths ...string) error {
	resolved := make([]string, 0, len(paths))
	seen := map[string]struct{}{}
	for _, p := range paths {
		if strings.TrimSpace(p) == "" {
			continue
		}
		r, err := ResolveRoot(p)
		if err != nil {
			return fmt.Errorf("vfs: control-plane path %q: %w", p, err)
		}
		if _, dup := seen[r]; dup {
			continue
		}
		seen[r] = struct{}{}
		resolved = append(resolved, r)
	}
	controlPlaneDenied.Store(&resolved)
	return nil
}

// ControlPlaneDenied returns the registered control-plane directories (resolved
// absolute paths), or nil when none is registered. The slice is a copy.
func ControlPlaneDenied() []string {
	p := controlPlaneDenied.Load()
	if p == nil {
		return nil
	}
	out := make([]string, len(*p))
	copy(out, *p)
	return out
}

// WithinControlPlane reports whether candidate resolves AT or UNDER any dir in
// deniedDirs, returning the matched denied dir when it does. Both the candidate
// AND each denied dir are symlink-resolved here (so a link INTO the control plane
// is caught by its real target, and a caller that passes an abs-but-unresolved
// dir still compares correctly) and the comparison is segment-aware (a sibling
// like ".contenox2" is NOT under ".contenox"). It is the pure predicate the grant
// verbs use with an EXPLICIT denied set: the `contenox workspace add` CLI runs in
// a process that never called SetControlPlaneDenied, so it computes the
// control-plane dirs itself and passes them here.
func WithinControlPlane(deniedDirs []string, candidate string) (string, bool) {
	if len(deniedDirs) == 0 {
		return "", false
	}
	resolved, err := ResolveRoot(candidate)
	if err != nil {
		return "", false
	}
	for _, d := range deniedDirs {
		rd, err := ResolveRoot(d)
		if err != nil {
			continue
		}
		if within(rd, resolved) {
			return d, true
		}
	}
	return "", false
}

// IsControlPlanePath reports whether candidate resolves AT or UNDER the
// PROCESS-GLOBAL control plane (SetControlPlaneDenied). Used inside serve, where
// the global is set — e.g. the REST grant verb. Equivalent to
// WithinControlPlane(ControlPlaneDenied(), candidate).
func IsControlPlanePath(candidate string) (string, bool) {
	return WithinControlPlane(ControlPlaneDenied(), candidate)
}

// deniedResolved checks an ALREADY-resolved absolute path against the global
// denylist. Its callers (Factory.Resolve, containWithin) have already
// symlink-resolved the path, so it does no further I/O — a pure segment-aware
// comparison. Returns the matched denied dir.
func deniedResolved(resolvedAbs string) (string, bool) {
	p := controlPlaneDenied.Load()
	if p == nil {
		return "", false
	}
	for _, d := range *p {
		if within(d, resolvedAbs) {
			return d, true
		}
	}
	return "", false
}

// controlPlaneError builds the teaching refusal, naming the boundary plainly so a
// model or operator learns WHY rather than decoding a status code.
func controlPlaneError(requested, deniedDir string) error {
	return fmt.Errorf("%w: %s is inside the runtime's control plane (%s), which is never a workspace — the runtime never lets an agent reach its own governing config, database, or policies", ErrControlPlane, requested, deniedDir)
}

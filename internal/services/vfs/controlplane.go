package vfs

import (
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
)

// ErrControlPlane is the sentinel every control-plane refusal wraps: a path at
// or under the runtime's own governing state is never a workspace, session cwd
// or resolvable subpath, even under a granted root and even via a symlink.
var ErrControlPlane = errors.New("path is inside the runtime control plane")

var controlPlaneDenied atomic.Pointer[[]string]

// SetControlPlaneDenied registers the runtime's control-plane directories;
// calling it with no paths clears the denylist. The denylist is process-global
// and survives every SetRoots hot-reload.
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

// WithinControlPlane reports whether candidate resolves at or under any dir
// in deniedDirs, returning the matched dir when it does. Both candidate and
// each denied dir are symlink-resolved and compared segment-aware.
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

// IsControlPlanePath reports whether candidate resolves at or under the
// process-global control plane (SetControlPlaneDenied).
func IsControlPlanePath(candidate string) (string, bool) {
	return WithinControlPlane(ControlPlaneDenied(), candidate)
}

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

func controlPlaneError(requested, deniedDir string) error {
	return fmt.Errorf("%w: %s is inside the runtime's control plane (%s), which is never a workspace — the runtime never lets an agent reach its own governing config, database, or policies", ErrControlPlane, requested, deniedDir)
}

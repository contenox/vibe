// Package vfs is the single home for workspace-root containment: normalizing a
// candidate path against a root and rejecting anything that escapes it, symlinks
// included. Two callers used to carry their own copy of this logic — the
// local_fs agent tool (runtime/localtools) and the /files browse API
// (runtime/localfileservice); both now delegate here so there is exactly one
// symlink-escape guard to reason about.
//
// The API is deliberately small:
//
//   - Contain(root, candidate) resolves a path within a root and is the core
//     primitive. It tolerates a non-existent leaf (resolving the deepest
//     existing parent) so create/write paths validate before any I/O.
//   - Within(root, abs) is the raw within-root predicate for callers that have
//     already resolved the absolute path themselves.
//   - Factory holds the serve-level allowlist of workspace roots and vends
//     rooted Views. A session may only operate within an allowlisted root.
//   - View is a Factory-vended, root-bound convenience wrapper over Contain.
package vfs

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ErrEscape is returned (wrapped) when a candidate path resolves outside its
// root. Callers translate it into their own domain error via errors.Is.
var ErrEscape = errors.New("path escapes workspace root")

// ErrNotAllowed is returned when a requested root is not in the Factory's
// allowlist.
var ErrNotAllowed = errors.New("workspace root is not allowed")

// ResolveRoot returns the cleaned, absolute, symlink-resolved form of root. A
// non-existent root is tolerated (its cleaned absolute form is returned) so a
// workspace directory that has not been created yet still validates.
func ResolveRoot(root string) (string, error) {
	if strings.TrimSpace(root) == "" {
		return "", errors.New("vfs: empty root")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return "", fmt.Errorf("vfs: invalid root: %w", err)
	}
	if real, err := filepath.EvalSymlinks(abs); err == nil {
		abs = real
	} else if !os.IsNotExist(err) {
		return "", fmt.Errorf("vfs: root resolution error: %w", err)
	}
	return filepath.Clean(abs), nil
}

// Contain resolves candidate (absolute, or relative to root) to a cleaned,
// symlink-resolved absolute path guaranteed to lie within root. Symlinks are
// followed so an in-sandbox link pointing outside the root is caught before any
// I/O. A non-existent leaf is permitted: the deepest existing parent is
// resolved and the missing suffix re-appended, so writing "link/new.txt" where
// "link" escapes is still rejected. Returns an error wrapping ErrEscape when the
// path leaves root.
func Contain(root, candidate string) (string, error) {
	realRoot, err := ResolveRoot(root)
	if err != nil {
		return "", err
	}
	return containWithin(realRoot, root, candidate)
}

func containWithin(realRoot, displayRoot, candidate string) (string, error) {
	return containWithinOpts(realRoot, displayRoot, candidate, false)
}

// containWithinOpts is containWithin with the privileged escape hatch for the
// runtime's OWN reads of its control plane (see OpenPrivilegedView). Escape
// containment is enforced unconditionally — privilege waives only the
// control-plane deny, never the root boundary.
func containWithinOpts(realRoot, displayRoot, candidate string, privileged bool) (string, error) {
	absPath := candidate
	if !filepath.IsAbs(candidate) {
		absPath = filepath.Join(realRoot, candidate)
	}
	absPath, err := filepath.Abs(absPath)
	if err != nil {
		return "", fmt.Errorf("vfs: invalid path: %w", err)
	}
	real, err := resolveLeaf(absPath)
	if err != nil {
		return "", fmt.Errorf("vfs: path resolution error: %w", err)
	}
	real = filepath.Clean(real)
	if !within(realRoot, real) {
		return "", fmt.Errorf("%w: %s escapes %s", ErrEscape, candidate, displayRoot)
	}
	// Control-plane carveout (vfs-invariant slice, 2026-07-21 — containment made
	// it live): a path that stays WITHIN a legitimate root is STILL refused when
	// it lands at or under the runtime's control plane. This is the traversal-side
	// half of the carveout (the root-selection half is Factory.Resolve): it closes
	// the /files explorer "enter .contenox" path and the local_fs agent tool, both
	// of which resolve a RELATIVE path inside a granted root and never touch the
	// Factory's root selection. `real` is already symlink-resolved above, so a link
	// inside the root pointing into the control plane is caught by its real target.
	// See controlplane.go.
	if !privileged {
		if denied, ok := deniedResolved(real); ok {
			return "", controlPlaneError(candidate, denied)
		}
	}
	return real, nil
}

// Within reports whether abs lies within root. root is symlink-resolved; abs is
// compared as given (callers that resolved it via EvalSymlinks pass the real
// path). Both are made absolute first.
func Within(root, abs string) bool {
	realRoot, err := ResolveRoot(root)
	if err != nil {
		return false
	}
	absPath, err := filepath.Abs(abs)
	if err != nil {
		return false
	}
	return within(realRoot, filepath.Clean(absPath))
}

func within(realRoot, abs string) bool {
	sep := string(filepath.Separator)
	rel, err := filepath.Rel(realRoot, abs)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+sep))
}

// resolveLeaf resolves symlinks for an existing target, and for a non-existing
// target resolves the deepest existing parent directory before appending the
// missing suffix. This prevents writes such as "link/new.txt" where "link" is a
// symlink escaping the sandbox.
func resolveLeaf(absPath string) (string, error) {
	absPath = filepath.Clean(absPath)

	if realPath, err := filepath.EvalSymlinks(absPath); err == nil {
		return filepath.Abs(realPath)
	} else if !os.IsNotExist(err) {
		return "", err
	}

	probe := absPath
	var missing []string

	for {
		realPath, err := filepath.EvalSymlinks(probe)
		if err == nil {
			resolved := realPath
			for i := len(missing) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, missing[i])
			}
			return filepath.Abs(resolved)
		}
		if !os.IsNotExist(err) {
			return "", err
		}

		parent := filepath.Dir(probe)
		if parent == probe {
			return "", err
		}

		missing = append(missing, filepath.Base(probe))
		probe = parent
	}
}

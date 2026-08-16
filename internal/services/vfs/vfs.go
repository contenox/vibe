// Package vfs is the single home for workspace-root containment: resolving a
// candidate path against a root and rejecting anything that escapes it, symlinks
// included. Factory holds the serve-level root allowlist and vends rooted Views.
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

// ResolveRoot returns the cleaned, absolute, symlink-resolved form of root.
// A non-existent root is tolerated: its cleaned absolute form is returned.
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

// Contain resolves candidate to a cleaned, symlink-resolved absolute path
// guaranteed to lie within root, wrapping ErrEscape when it does not. A
// non-existent leaf is permitted.
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
	// real is already symlink-resolved, so a link into the control plane is
	// caught here too.
	if !privileged {
		if denied, ok := deniedResolved(real); ok {
			return "", controlPlaneError(candidate, denied)
		}
	}
	return real, nil
}

// Within reports whether abs lies within root. root is symlink-resolved; abs
// is compared as given (callers that resolved it via EvalSymlinks pass the
// real path). Both are made absolute first.
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

func resolveLeaf(absPath string) (string, error) {
	absPath = filepath.Clean(absPath)

	if realPath, err := filepath.EvalSymlinks(absPath); err == nil {
		return filepath.Abs(realPath)
	} else if !os.IsNotExist(err) {
		return "", err
	}

	// EvalSymlinks reports ENOENT for both a missing path and a symlink whose
	// target is missing. Lstat tells them apart, and a dangling link is refused
	// rather than treated as a plain missing file that re-appends inside the root.
	if fi, lerr := os.Lstat(absPath); lerr == nil && fi.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("%w: %s is a symlink whose target cannot be resolved", ErrEscape, absPath)
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

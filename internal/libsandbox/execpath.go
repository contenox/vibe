package libsandbox

import (
	"fmt"
	"path/filepath"
	"strings"
)

// SystemExecDirs is the single source of truth for the confined PATH: stock
// binary locations, each a subtree of the read+execute system runtime the
// wall already grants (see systemRuntimePaths in landlock_linux.go). scrubEnv
// joins these into the emulated PATH and validatePATH admits an entry only if
// it lies within one of these or a named carve-out — keeping "findable by
// name" and "granted by the wall" in sync. Every entry must stay within
// systemRuntimePaths (guarded by the Linux
// TestUnit_SystemExecDirs_WithinLandlockExecSurface test). Order is PATH
// precedence (earliest wins).
func SystemExecDirs() []string {
	return []string{
		"/usr/local/sbin",
		"/usr/local/bin",
		"/usr/sbin",
		"/usr/bin",
		"/sbin",
		"/bin",
	}
}

// canonicalPATH is the minimal fallback PATH: SystemExecDirs joined, with no
// profile-derived entry. It is the floor confinedPATH falls back to rather
// than producing an empty PATH.
func canonicalPATH() string {
	return strings.Join(SystemExecDirs(), string(filepath.ListSeparator))
}

// confinedPATH is the operator's PATH filtered to the exec surface: only
// entries within SystemExecDirs or a (tilde-resolved) FS carve-out survive,
// order and de-duplication preserved. This keeps PATH ⊆ exec surface by
// construction — a carved toolchain dir (e.g. ~/.nvm/…) resolves, an uncarved
// profile dir (~/.cargo/bin) does not — which is what makes validatePATH a
// tautology on the default and a real check only on an explicit EnvSet
// override. Relative and empty entries are dropped. Falls back to
// canonicalPATH if nothing survives.
func confinedPATH(operatorPATH, home string, fs []FSCarveout) string {
	roots := make([]string, 0, len(SystemExecDirs())+len(fs))
	roots = append(roots, SystemExecDirs()...)
	for _, c := range fs {
		roots = append(roots, resolveTilde(c.Path, home))
	}
	seen := make(map[string]struct{})
	kept := make([]string, 0)
	for _, entry := range filepath.SplitList(operatorPATH) {
		if entry == "" || !filepath.IsAbs(entry) {
			continue
		}
		clean := filepath.Clean(entry)
		if _, dup := seen[clean]; dup {
			continue
		}
		if pathWithin(clean, roots) {
			seen[clean] = struct{}{}
			kept = append(kept, clean)
		}
	}
	if len(kept) == 0 {
		return canonicalPATH()
	}
	return strings.Join(kept, string(filepath.ListSeparator))
}

// validatePATH hard-rejects a confined PATH that reaches outside the wall:
// a caller may override the default canonicalPATH via EnvSet["PATH"], and
// that override is only sound if every entry is absolute and lies within
// SystemExecDirs or a (tilde-resolved) FS carve-out. A violation wraps
// ErrInvalidSpec so Command fails before the wall is built, rather than an
// opaque EACCES from Landlock later. The workspace is deliberately not part
// of the surface: a confined PATH must never make a just-written workspace
// file runnable by bare name. An empty PATH is inert.
func validatePATH(pathValue, home string, fs []FSCarveout) error {
	if strings.TrimSpace(pathValue) == "" {
		return nil
	}
	roots := make([]string, 0, len(SystemExecDirs())+len(fs))
	roots = append(roots, SystemExecDirs()...)
	for _, c := range fs {
		roots = append(roots, resolveTilde(c.Path, home))
	}
	for _, entry := range filepath.SplitList(pathValue) {
		if entry == "" {
			return fmt.Errorf("%w: PATH has an empty entry, which resolves to the current directory — a bare-name exec of a workspace file; every confined PATH entry must be an absolute directory within the wall", ErrInvalidSpec)
		}
		if !filepath.IsAbs(entry) {
			return fmt.Errorf("%w: PATH entry %q is not absolute; a confined PATH must name absolute directories within the wall", ErrInvalidSpec, entry)
		}
		if !pathWithin(entry, roots) {
			return fmt.Errorf("%w: PATH entry %q is outside the confined exec surface (the system exec dirs and the declared FS carve-outs); add an FS carve-out for it or drop it from EnvSet[\"PATH\"]", ErrInvalidSpec, entry)
		}
	}
	return nil
}

// pathWithin reports whether p is at or beneath one of roots. Comparison is
// segment-wise (via a trailing separator), so "/usr/bin" is within "/usr"
// while "/usrlocal" is not.
func pathWithin(p string, roots []string) bool {
	p = filepath.Clean(p)
	for _, r := range roots {
		r = filepath.Clean(r)
		if p == r || strings.HasPrefix(p, r+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

// lookupEnv returns the value of name in a "KEY=VALUE"-shaped env slice, or ""
// if absent. On duplicates, the last wins (POSIX exec semantics).
func lookupEnv(env []string, name string) string {
	prefix := name + "="
	value := ""
	for _, kv := range env {
		if strings.HasPrefix(kv, prefix) {
			value = kv[len(prefix):]
		}
	}
	return value
}

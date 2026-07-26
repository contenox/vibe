package libsandbox

import (
	"fmt"
	"path/filepath"
	"strings"
)

// SystemExecDirs is the canonical set of directories the confined PATH is built
// from: the stock binary locations, every one a subtree of the read+execute
// system runtime the wall already grants (see systemRuntimePaths in
// landlock_linux.go). It is the SINGLE source of truth the two halves of the exec
// story derive from — scrubEnv joins these into the emulated PATH, and
// validatePATH admits a PATH entry only if it lies within one of these (or a named
// carve-out). Tying the confined PATH and the Landlock exec surface to one list is
// what closes the classic mismatch this whole change exists to kill: a binary
// findable by name but denied by the wall, or granted by the wall yet invisible on
// PATH. Every entry MUST stay within systemRuntimePaths so the wall actually grants
// it — the Linux TestUnit_SystemExecDirs_WithinLandlockExecSurface guards that.
//
// Order is PATH precedence (earliest wins), matching the stock login-shell PATH.
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

// canonicalPATH is the minimal fallback PATH: SystemExecDirs joined with the list
// separator, carrying NO profile-derived entry. It is what a confined process gets
// when the operator's PATH filters down to nothing (see confinedPATH) — a floor
// that at least reaches the stock system binaries, never an empty PATH.
func canonicalPATH() string {
	return strings.Join(SystemExecDirs(), string(filepath.ListSeparator))
}

// confinedPATH is the PATH a confined process actually gets: the operator's PATH
// FILTERED to the exec surface — the entries that lie within SystemExecDirs or a
// (tilde-resolved) FS carve-out — with order and de-duplication preserved. It is
// the reconciliation of two truths that a bare canonical PATH got wrong: a confined
// agent must find its real toolchain (node under a carved ~/.nvm, ripgrep under a
// carved dir), yet must not be steered by a profile dir the wall never granted.
//
// Filtering delivers both. A dir the operator put on PATH survives IFF the wall
// also grants it — so binary lookup exactly follows the carve-out necessity list:
// carve a dir and its executables become findable; leave it uncarved and it is
// neither reachable nor findable. An uncarved profile dir (~/.cargo/bin) is dropped,
// preserving the "no findable-but-denied dir" invariant, while a carved toolchain
// dir (~/.nvm/versions/node/X/bin, within carved ~/.nvm) is kept, so node resolves.
// This keeps PATH ⊆ exec surface by construction, which is what makes validatePATH
// a tautology on the default and a real check only on an explicit EnvSet override.
//
// Order is the operator's own (their profile already prepends a version-manager's
// node ahead of any system node, and that precedence is preserved). Relative and
// empty entries are dropped — a confined PATH names absolute dirs only. If nothing
// survives (an operator PATH with no granted dir, or an empty one), it falls back
// to canonicalPATH so the process still reaches the stock system binaries.
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

// validatePATH hard-rejects a confined PATH that reaches outside the wall. It is
// the fail-closed half of the emulated-PATH design: scrubEnv builds canonicalPATH
// by default, but a caller may override it via EnvSet["PATH"] to add a toolchain
// dir — and that override is only sound if the dir is actually granted. Every
// entry must be absolute and lie within SystemExecDirs or a (tilde-resolved) FS
// carve-out; anything else wraps ErrInvalidSpec so Command fails BEFORE the wall
// is built, rather than letting the mismatch surface later as an opaque EACCES
// from Landlock. The rejected shapes are exactly the dangerous ones:
//
//   - a relative or empty entry — an empty PATH element is the implicit current
//     directory, i.e. bare-name execution of a workspace file the agent just wrote;
//   - a profile dir like ~/.cargo/bin with no matching carve-out — the profile leak
//     this change removes.
//
// The workspace is deliberately NOT part of the surface: a confined PATH must
// never make a just-written workspace file runnable by bare name; the agent runs
// such a file by explicit path. An empty PATH is inert (nothing to run by name).
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

// pathWithin reports whether p is at or beneath one of roots, comparing cleaned
// paths segment-wise (a trailing separator on the root) so "/usr/bin" is within
// "/usr" while "/usrlocal" is not — a plain string prefix would wrongly admit the
// latter.
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

// lookupEnv returns the value of name in an "KEY=VALUE"-shaped env slice, or "" if
// absent. scrubEnv de-duplicates by name, so a single match is expected; if a
// caller passes a raw slice with duplicates, the last wins (POSIX exec semantics).
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

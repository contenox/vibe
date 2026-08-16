package libsandbox

import (
	"fmt"
	"path/filepath"
	"strings"
)

// SystemExecDirs returns the confined PATH's stock binary locations in
// precedence order, each a subtree of the Landlock-granted system runtime.
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

func canonicalPATH() string {
	return strings.Join(SystemExecDirs(), string(filepath.ListSeparator))
}

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

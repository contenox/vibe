package libsandbox

import (
	"sort"
	"strings"
)

// scrubEnv builds the minimal environment the confined process may inherit.
// Only names in allow (matched exactly) are copied from parentEnv, set is
// layered on top for explicit extras, PATH is forced to the emulated
// canonicalPATH (unless set names one), and HOME is always forced to home.
//
// Precedence, last wins: allow-copied < set < forced PATH/HOME. HOME is
// forced unconditionally; PATH only unless the caller opts into an explicit
// EnvSet override — these two are the mechanism keeping ~/.ssh, ~/.aws, and
// the operator's profile-built PATH out of reach, not overridable defaults.
//
// Result is sorted by name (pure, deterministic). Malformed parent entries
// (no "=") are skipped. home is used as given; Command validates it is
// non-empty.
func scrubEnv(parentEnv []string, allow []string, set map[string]string, home string) []string {
	allowed := make(map[string]struct{}, len(allow))
	for _, name := range allow {
		allowed[name] = struct{}{}
	}

	out := make(map[string]string, len(allow)+len(set)+2)
	for _, kv := range parentEnv {
		eq := strings.IndexByte(kv, '=')
		if eq < 0 {
			continue // no name to match; not a real KEY=VALUE entry
		}
		name := kv[:eq]
		if _, ok := allowed[name]; ok {
			out[name] = kv[eq+1:]
		}
	}
	for k, v := range set {
		out[k] = v
	}
	// PATH is forced to canonicalPATH, not inherited (an allow-copied PATH
	// would be the operator's profile-built value), unless the caller set one
	// explicitly via EnvSet — Command validates that override against the
	// carve-outs (see validatePATH). Unlike HOME, PATH yields to an explicit
	// override because a toolchain in a named carve-out is a legitimate,
	// checkable widening.
	if _, ok := set["PATH"]; !ok {
		out["PATH"] = canonicalPATH()
	}
	// HOME is forced unconditionally to the scoped home (see Spec.Home).
	out["HOME"] = home

	names := make([]string, 0, len(out))
	for name := range out {
		names = append(names, name)
	}
	sort.Strings(names)

	env := make([]string, 0, len(names))
	for _, name := range names {
		env = append(env, name+"="+out[name])
	}
	return env
}

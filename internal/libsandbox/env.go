package libsandbox

import (
	"sort"
	"strings"
)

// scrubEnv builds the minimal environment the confined process is allowed to
// inherit — the core of the credential-leak fix. The starting point is nothing:
// a bare exec.Command that appends os.Environ() hands the child every secret in
// the parent (AWS keys, tokens, npm creds), which is exactly what a compromised
// postinstall script exfiltrates. Instead only the names in allow (matched
// exactly) are copied out of parentEnv, then set is layered on for explicit
// extras, PATH is forced to the emulated canonicalPATH (unless set names one), and
// finally HOME is forced to home.
//
// Precedence, last wins: an allow-copied value < a set value < forced PATH/HOME.
// So a caller can pass a variable through by name yet override it explicitly, but
// the emulated PATH and the scoped HOME are authoritative — HOME unconditionally,
// PATH unless the caller opts into an explicit EnvSet override. They are the
// mechanism that keeps ~/.ssh, ~/.aws, ~/.contenox, and the operator's
// profile-built PATH out of reach, not defaults to be overridden.
//
// The result is sorted by name, so the same inputs always produce the same
// slice: the function is pure and deterministic, which keeps it testable and
// keeps the emitted environment stable across runs. Malformed parent entries
// (no "=") are skipped — there is no name to match them by. home is used as
// given; validating that it is non-empty is Command's job.
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
	// PATH is emulated on the confined path, not inherited. Even an allow-copied
	// PATH is the operator's profile-built value (~/.cargo/bin, ~/.local/bin, the
	// /etc/profile.d additions) and must not steer the agent's binary lookup
	// outside the wall, so it is forced to canonicalPATH — unless the caller set
	// one explicitly via EnvSet, an opt-in extension Command validates against the
	// carve-outs (see validatePATH). This mirrors the HOME split: like the scoped
	// HOME, the canonical PATH keeps a profile-derived value out of reach; unlike
	// HOME it yields to an explicit EnvSet, because a toolchain in a named carve-out
	// is a legitimate, checkable widening. The native-shell path (EnvPolicy.Apply)
	// does not force PATH — those shells run in the operator's real environment.
	if _, ok := set["PATH"]; !ok {
		out["PATH"] = canonicalPATH()
	}
	// HOME is forced to the scoped home dir, overriding anything inherited or
	// set: the scoped HOME is the whole reason ~/.ssh, ~/.aws, ~/.npmrc, and
	// ~/.contenox are not reachable (see Spec.Home).
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

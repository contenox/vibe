package libsandbox

import (
	"sort"
	"strings"
)

// OverlayEnv returns env ("KEY=VALUE" entries) with the pairs in set applied on
// top: a name already present has its value replaced, a name not present is
// added. It is the injection counterpart to a scrub filter — where a policy
// decides what an inherited environment KEEPS, OverlayEnv is how an operator
// deliberately ADDS or overrides variables, and those explicit values always win
// over whatever was inherited or passed by the policy. A set value of "" is kept
// ("KEY="), which exports a defined-but-empty variable.
//
// The result is de-duplicated by name (last inherited value wins before the
// overlay, matching exec semantics) and sorted, so identical inputs yield an
// identical slice. Malformed env entries (no "=") are dropped — there is no name
// to key them by. An empty set is a no-op: env is returned unchanged, so callers
// can overlay unconditionally without perturbing an environment they had no
// variables to add to.
func OverlayEnv(env []string, set map[string]string) []string {
	if len(set) == 0 {
		return env
	}
	merged := make(map[string]string, len(env)+len(set))
	for _, kv := range env {
		if eq := strings.IndexByte(kv, '='); eq >= 0 {
			merged[kv[:eq]] = kv[eq+1:]
		}
	}
	for k, v := range set {
		merged[k] = v
	}
	names := make([]string, 0, len(merged))
	for name := range merged {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]string, len(names))
	for i, name := range names {
		out[i] = name + "=" + merged[name]
	}
	return out
}

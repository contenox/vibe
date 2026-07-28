package libsandbox

import (
	"sort"
	"strings"
)

// OverlayEnv returns env ("KEY=VALUE" entries) with the pairs in set applied
// on top, replacing or adding names; set values always win over inherited
// ones. A set value of "" exports a defined-but-empty variable ("KEY=").
//
// Result is de-duplicated by name (last inherited value wins pre-overlay,
// matching exec semantics) and sorted. Malformed env entries (no "=") are
// dropped. An empty set returns env unchanged.
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

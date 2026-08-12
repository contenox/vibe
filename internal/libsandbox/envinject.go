package libsandbox

import (
	"sort"
	"strings"
)

// OverlayEnv returns env with the pairs in set applied on top (set always wins), de-duplicated by name and sorted; malformed entries (no "=") are dropped and an empty set returns env unchanged.
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

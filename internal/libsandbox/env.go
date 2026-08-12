package libsandbox

import (
	"sort"
	"strings"
)

func scrubEnv(parentEnv []string, allow []string, set map[string]string, home string) []string {
	allowed := make(map[string]struct{}, len(allow))
	for _, name := range allow {
		allowed[name] = struct{}{}
	}

	out := make(map[string]string, len(allow)+len(set)+2)
	for _, kv := range parentEnv {
		eq := strings.IndexByte(kv, '=')
		if eq < 0 {
			continue
		}
		name := kv[:eq]
		if _, ok := allowed[name]; ok {
			out[name] = kv[eq+1:]
		}
	}
	for k, v := range set {
		out[k] = v
	}
	// PATH is forced to canonicalPATH unless the caller overrides it via EnvSet (validated against carve-outs); HOME is always forced and never overridable.
	if _, ok := set["PATH"]; !ok {
		out["PATH"] = canonicalPATH()
	}
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

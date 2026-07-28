package libsandbox

import (
	"sort"
	"strings"
	"unicode"
)

// EnvPolicy is the operator-facing whitelist deciding which parent
// environment variables a confined process may inherit. It sits above
// scrubEnv: Spec.EnvAllow is the resolved exact-name list scrubEnv copies;
// EnvPolicy is what an operator authors (exact names or simple globs), with
// a Deny set that always wins so the control plane and obvious credential
// shapes can never ride through even a careless-broad Allow. Resolve expands
// the policy against a concrete parent environment into that exact list:
//
//	spec.EnvAllow = libsandbox.DefaultEnvPolicy().
//		Allowing(libsandbox.ParseEnvList(os.Getenv("CONTENOX_SANDBOX_ENV_ALLOW"))...).
//		Resolve(os.Environ())
//
// The default answer is still "no hole": DefaultEnvPolicy allows only the
// handful of variables a POSIX shell needs, none carrying a secret.
type EnvPolicy struct {
	// Allow lists names or globs to pass through. A glob has a single
	// leading or trailing "*" ("LC_*", "*_PROXY"); a bare "*" passes
	// everything Deny does not veto.
	Allow []string
	// Deny lists names or globs that are never passed, even when Allow
	// matches — Deny wins. Defaults to the control plane plus common
	// credential shapes, so e.g. widening Allow to "AWS_*" still cannot
	// leak AWS_SECRET_ACCESS_KEY.
	Deny []string
}

// DefaultEnvAllow is the sane baseline: what a POSIX shell and common
// toolchains need, nothing that carries a secret. HOME is deliberately
// absent — scrubEnv always forces the scoped HOME. Toolchain/deployment
// specifics (GOCACHE, HTTP(S)_PROXY, …) are not defaulted; an operator adds
// them knowingly. PATH is inherited on the native-shell path (EnvPolicy.Apply)
// but neutralized on the confined path, where scrubEnv forces the emulated
// canonical PATH over any inherited value.
func DefaultEnvAllow() []string {
	return []string{
		"PATH",
		"TERM",
		"COLORTERM",
		"TZ",
		"LANG",
		"LANGUAGE",
		"LC_*",
		"TMPDIR",
		"USER",
		"LOGNAME",
		"SHELL",
	}
}

// ControlPlaneEnvDeny is the non-negotiable veto for contenox's own
// control-plane variables — unlike credential-shape patterns, no wider
// Allow should ever be able to re-permit these.
func ControlPlaneEnvDeny() []string {
	return []string{"CONTENOX_*"}
}

// SecretEnvDeny is the common credential name-shape veto, letting a loose
// "pass everything" allow still strip obvious secrets (e.g. the suffix glob
// catches AWS_SECRET_ACCESS_KEY while a sibling like AWS_REGION still
// passes).
func SecretEnvDeny() []string {
	return []string{
		"*_TOKEN",
		"*_KEY",
		"*_SECRET",
		"*_PASSWORD",
		"*_PASSWD",
		"*_CREDENTIALS",
	}
}

// DefaultEnvDeny is the union of ControlPlaneEnvDeny and SecretEnvDeny used
// by DefaultEnvPolicy.
func DefaultEnvDeny() []string {
	return append(ControlPlaneEnvDeny(), SecretEnvDeny()...)
}

// DefaultEnvPolicy is the recommended starting point: DefaultEnvAllow gated
// by DefaultEnvDeny.
func DefaultEnvPolicy() EnvPolicy {
	return EnvPolicy{Allow: DefaultEnvAllow(), Deny: DefaultEnvDeny()}
}

// Allowing returns a copy of p with names appended to Allow; it copies
// rather than mutates so DefaultEnvPolicy's shared slices are never aliased.
func (p EnvPolicy) Allowing(names ...string) EnvPolicy {
	return EnvPolicy{Allow: concat(p.Allow, names), Deny: clone(p.Deny)}
}

// Denying returns a copy of p with names appended to Deny.
func (p EnvPolicy) Denying(names ...string) EnvPolicy {
	return EnvPolicy{Allow: clone(p.Allow), Deny: concat(p.Deny, names)}
}

// Resolve expands the policy against a parent environment ("KEY=VALUE"
// entries) into the sorted, de-duplicated names Spec.EnvAllow expects: every
// name matching an Allow rule and no Deny rule. Only names present in
// parentEnv appear. Malformed entries (no "=") are skipped.
func (p EnvPolicy) Resolve(parentEnv []string) []string {
	kept := make(map[string]struct{}, len(p.Allow))
	for _, kv := range parentEnv {
		if eq := strings.IndexByte(kv, '='); eq >= 0 && p.passes(kv[:eq]) {
			kept[kv[:eq]] = struct{}{}
		}
	}
	names := make([]string, 0, len(kept))
	for name := range kept {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// Apply is the counterpart used by native shell-exec sites (local_shell tool,
// the "!" PTY): where Resolve returns names, Apply returns the surviving
// "KEY=VALUE" entries ready for exec.Cmd.Env. Unlike scrubEnv it does not
// force a scoped HOME — these shells run in the operator's real environment.
// Last occurrence of a name wins; result is sorted. Malformed entries (no
// "=") are skipped.
func (p EnvPolicy) Apply(parentEnv []string) []string {
	kept := make(map[string]string, len(parentEnv))
	for _, kv := range parentEnv {
		eq := strings.IndexByte(kv, '=')
		if eq < 0 {
			continue
		}
		if name := kv[:eq]; p.passes(name) {
			kept[name] = kv // last wins
		}
	}
	names := make([]string, 0, len(kept))
	for name := range kept {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]string, len(names))
	for i, name := range names {
		out[i] = kept[name]
	}
	return out
}

// passes reports whether name matches an Allow rule and no Deny rule (Deny
// wins).
func (p EnvPolicy) passes(name string) bool {
	return !matchesAny(p.Deny, name) && matchesAny(p.Allow, name)
}

// ParseEnvList splits an operator-supplied allow/deny list (comma,
// semicolon, or whitespace separated) into entries, trimming blanks. It does
// not validate glob syntax, so a typo simply matches nothing and fails
// closed. Returns nil for an empty or all-blank input.
func ParseEnvList(s string) []string {
	fields := strings.FieldsFunc(s, func(r rune) bool {
		return r == ',' || r == ';' || unicode.IsSpace(r)
	})
	if len(fields) == 0 {
		return nil
	}
	return fields
}

// matchesAny reports whether name matches any pattern (see envNameMatches).
func matchesAny(patterns []string, name string) bool {
	for _, pat := range patterns {
		if envNameMatches(pat, name) {
			return true
		}
	}
	return false
}

// envNameMatches reports whether name matches pattern: an exact name, or a
// name with a single leading or trailing "*" (prefix/suffix match), or a
// bare "*" matching everything. Case-sensitive. An empty pattern matches
// nothing.
func envNameMatches(pattern, name string) bool {
	switch {
	case pattern == "":
		return false
	case pattern == "*":
		return true
	case pattern == name:
		return true
	case strings.HasPrefix(pattern, "*"):
		return strings.HasSuffix(name, pattern[1:])
	case strings.HasSuffix(pattern, "*"):
		return strings.HasPrefix(name, pattern[:len(pattern)-1])
	default:
		return false
	}
}

func clone(s []string) []string {
	if len(s) == 0 {
		return nil
	}
	out := make([]string, len(s))
	copy(out, s)
	return out
}

func concat(a, b []string) []string {
	out := make([]string, 0, len(a)+len(b))
	out = append(out, a...)
	out = append(out, b...)
	return out
}

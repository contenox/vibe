package libsandbox

import (
	"sort"
	"strings"
	"unicode"
)

// EnvPolicy is the operator-facing whitelist (exact names or simple globs) deciding which parent environment variables a confined process may inherit, with Deny always winning over Allow.
type EnvPolicy struct {
	// Allow lists names or globs to pass through (a glob has a single leading or trailing "*"; a bare "*" passes everything Deny does not veto).
	Allow []string
	// Deny lists names or globs that are never passed even when Allow matches — Deny always wins.
	Deny []string
}

// DefaultEnvAllow is the sane baseline (POSIX-shell essentials, nothing that carries a secret); HOME is deliberately absent since scrubEnv always forces the scoped HOME.
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

// SecretEnvDeny is the common credential name-shape veto, letting a loose "pass everything" allow still strip obvious secrets.
func SecretEnvDeny() []string {
	return []string{
		"*_TOKEN",
		"*_KEY",
		"*_SECRET",
		"*_PASSWORD",
		"*_PASSWD",
		"*_CREDENTIALS",
		"*_API_KEY_ID",
		"AWS_ACCESS_KEY_ID",
		"AWS_SESSION_TOKEN",
		"PGPASSWORD",
		"PGPASSFILE",
		"MYSQL_PWD",
		"*_PWD",
		"DATABASE_URL",
		"*_DATABASE_URL",
		"REDIS_URL",
		"MONGODB_URI",
		"*_DSN",
		"SSH_AUTH_SOCK",
		"KUBECONFIG",
		"*_AUTH",
		"*_PAT",
		"NETRC",
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

// Resolve expands the policy against a parent environment into the sorted, de-duplicated names (matching Allow, not Deny) that are present in parentEnv; malformed entries (no "=") are skipped.
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

// Apply is Resolve's counterpart for native shell-exec sites, returning surviving "KEY=VALUE" entries (last occurrence wins, sorted) instead of names; unlike scrubEnv it does not force a scoped HOME.
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

func envNameVetoes(pattern, name string) bool {
	return envNameMatches(strings.ToUpper(pattern), strings.ToUpper(name))
}

func vetoesAny(patterns []string, name string) bool {
	for _, p := range patterns {
		if envNameVetoes(p, name) {
			return true
		}
	}
	return false
}

func (p EnvPolicy) passes(name string) bool {
	return !vetoesAny(p.Deny, name) && matchesAny(p.Allow, name)
}

// ParseEnvList splits an operator-supplied allow/deny list (comma/semicolon/whitespace separated) into trimmed entries, returning nil for empty or all-blank input; it does not validate glob syntax, so a typo simply matches nothing.
func ParseEnvList(s string) []string {
	fields := strings.FieldsFunc(s, func(r rune) bool {
		return r == ',' || r == ';' || unicode.IsSpace(r)
	})
	if len(fields) == 0 {
		return nil
	}
	return fields
}

func matchesAny(patterns []string, name string) bool {
	for _, pat := range patterns {
		if envNameMatches(pat, name) {
			return true
		}
	}
	return false
}

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

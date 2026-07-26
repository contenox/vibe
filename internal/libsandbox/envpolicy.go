package libsandbox

import (
	"sort"
	"strings"
	"unicode"
)

// EnvPolicy is the operator-facing whitelist that decides which of the parent
// process's environment variables a confined process may inherit. It sits one
// layer above scrubEnv: where Spec.EnvAllow is the resolved, EXACT list scrubEnv
// copies (and whose documented default is to pass nothing), EnvPolicy is what a
// human actually authors — a sane default set plus their own additions, written
// as exact names OR simple globs, with a Deny set that always wins so the control
// plane and the obvious credential shapes can never ride through even a
// careless-broad Allow.
//
// It changes none of scrubEnv's guarantees. A variable is inherited only if it
// matches an Allow rule AND no Deny rule; everything else is absent by
// construction, exactly as the wall intends. Resolve turns the policy — against a
// concrete parent environment — into the exact names Spec.EnvAllow takes:
//
//	spec.EnvAllow = libsandbox.DefaultEnvPolicy().
//		Allowing(libsandbox.ParseEnvList(os.Getenv("CONTENOX_SANDBOX_ENV_ALLOW"))...).
//		Resolve(os.Environ())
//
// The layering is deliberate: scrubEnv stays a pure exact-match mechanism (a glob
// placed directly in EnvAllow would match nothing), and the glob/default policy
// lives here where operators can reason about it. The default answer is still "no
// hole" — DefaultEnvPolicy allows only the handful of variables a POSIX shell and
// common toolchains need to run, none of which carries a secret; a build agent
// that needs GOCACHE, CARGO_HOME, or a proxy passes those explicitly, they are
// not defaulted in.
type EnvPolicy struct {
	// Allow lists names or globs to pass through from the parent environment. A
	// glob is a name with a single leading or trailing "*": "LC_*" matches
	// LC_ALL / LC_CTYPE, "*_PROXY" matches HTTP_PROXY / HTTPS_PROXY. A bare "*"
	// passes everything the Deny set does not veto — an explicit "I trust this
	// environment, just strip the known secrets" mode, discouraged by default.
	Allow []string
	// Deny lists names or globs that are never passed, even when Allow matches —
	// Deny wins. It defaults to the control plane and the common credential
	// shapes, so widening Allow (say "AWS_*" to get AWS_REGION) still cannot leak
	// AWS_SECRET_ACCESS_KEY or AWS_SESSION_TOKEN.
	Deny []string
}

// DefaultEnvAllow is the sane baseline: the variables a POSIX shell and common
// toolchains read to behave, and nothing that carries a secret. HOME is
// deliberately absent — scrubEnv forces the scoped HOME, and letting it be
// inherited would defeat that. Toolchain and deployment specifics (GOCACHE,
// CARGO_HOME, HTTP(S)_PROXY, …) are NOT defaulted: they are a per-deployment
// passthrough an operator adds knowingly, because the default answer is no hole.
//
// PATH is listed but plays two roles by consumer. On the native-shell path
// (EnvPolicy.Apply — the local_shell tool, the "!" PTY) it is inherited, because
// those shells run in the operator's real environment. On the confined path
// (scrubEnv) it is neutralized: scrubEnv forces the emulated canonicalPATH over
// any inherited value, so a foreign agent never inherits the operator's
// profile-built PATH. Keeping PATH here therefore serves the native path without
// widening the confined one.
func DefaultEnvAllow() []string {
	return []string{
		"PATH",      // find binaries — a shell without it can run almost nothing
		"TERM",      // terminal type; sane output and curses behavior
		"COLORTERM", // truecolor capability hint
		"TZ",        // local timezone for date/time output
		"LANG",      // primary locale
		"LANGUAGE",  // GNU gettext locale fallback list
		"LC_*",      // per-category locale overrides (LC_ALL, LC_CTYPE, …)
		"TMPDIR",    // where tools write scratch files
		"USER",      // current user name (git, tools read it)
		"LOGNAME",   // POSIX spelling of the same
		"SHELL",     // login shell path (git, less, tools resolve it)
	}
}

// ControlPlaneEnvDeny is the non-negotiable veto: contenox's own control-plane
// variables. It belongs in EVERY scrubbing posture — the control plane must never
// ride into a spawned shell — and, unlike the credential-shape patterns, it is
// not something a wider Allow should ever be able to re-permit.
func ControlPlaneEnvDeny() []string {
	return []string{"CONTENOX_*"}
}

// SecretEnvDeny is the common credential name-shapes: the belt-and-suspenders
// veto that lets a loose "pass everything" allow still strip the obvious secrets.
// The suffix globs catch the credential WITHIN a family (AWS_SECRET_ACCESS_KEY,
// AWS_SESSION_TOKEN) while a deliberately-allowed benign sibling (AWS_REGION)
// passes. It is what the deny-secrets posture layers on; a strict allowlist does
// not need it — a non-allowed secret is already absent — which is precisely what
// lets strict re-permit one trusted credential by naming it in Allow.
func SecretEnvDeny() []string {
	return []string{
		"*_TOKEN",       // GITHUB_TOKEN, AWS_SESSION_TOKEN, NPM_TOKEN, …
		"*_KEY",         // ANTHROPIC_API_KEY, OPENAI_API_KEY, AWS_SECRET_ACCESS_KEY, …
		"*_SECRET",      // *_CLIENT_SECRET and friends
		"*_PASSWORD",    // database / registry passwords
		"*_PASSWD",      // the shorter spelling
		"*_CREDENTIALS", // GOOGLE_APPLICATION_CREDENTIALS, …
	}
}

// DefaultEnvDeny is the always-wins veto used by DefaultEnvPolicy: the control
// plane plus the common credential shapes. It is the union of ControlPlaneEnvDeny
// and SecretEnvDeny — the deny-secrets posture — kept as one call for the strict
// default policy, where the extra credential-shape denial is harmless
// defense-in-depth.
func DefaultEnvDeny() []string {
	return append(ControlPlaneEnvDeny(), SecretEnvDeny()...)
}

// DefaultEnvPolicy is the recommended starting point for a confined process:
// DefaultEnvAllow gated by DefaultEnvDeny. Callers extend it with Allowing /
// Denying rather than assembling a list from scratch.
func DefaultEnvPolicy() EnvPolicy {
	return EnvPolicy{Allow: DefaultEnvAllow(), Deny: DefaultEnvDeny()}
}

// Allowing returns a copy of p with names (exact or glob) appended to Allow — the
// operator passthrough. It copies rather than mutates so DefaultEnvPolicy's
// shared slices are never aliased by one caller's additions.
func (p EnvPolicy) Allowing(names ...string) EnvPolicy {
	return EnvPolicy{Allow: concat(p.Allow, names), Deny: clone(p.Deny)}
}

// Denying returns a copy of p with names (exact or glob) appended to Deny.
func (p EnvPolicy) Denying(names ...string) EnvPolicy {
	return EnvPolicy{Allow: clone(p.Allow), Deny: concat(p.Deny, names)}
}

// Resolve expands the policy against a parent environment (os.Environ() shape:
// "KEY=VALUE" entries) into the concrete, exact, sorted, de-duplicated variable
// names Spec.EnvAllow expects: every parent variable whose name matches an Allow
// rule and no Deny rule. Only names actually present in parentEnv appear — you
// cannot pass through a variable that is not set. Pure and deterministic, so the
// resolved allow list is stable and testable. Malformed parent entries (no "=")
// are skipped: there is no name to match them by.
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

// Apply is the counterpart the native shell-exec sites use — the local_shell
// tool, the terminal / "!" PTY. Where Resolve returns NAMES for a confined
// foreign agent's Spec.EnvAllow, Apply returns the surviving "KEY=VALUE" entries
// themselves, ready to assign to exec.Cmd.Env, and — unlike scrubEnv — it does
// NOT force a scoped HOME: these shells run in the operator's real environment,
// so HOME and everything else pass through the policy like any other variable.
// The last occurrence of a name wins (POSIX exec semantics) and the result is
// sorted by name, so identical inputs yield an identical, deterministic slice.
// Malformed parent entries (no "=") are skipped.
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

// passes reports whether a variable name survives the policy: it matches an Allow
// rule and no Deny rule — Deny wins.
func (p EnvPolicy) passes(name string) bool {
	return !matchesAny(p.Deny, name) && matchesAny(p.Allow, name)
}

// ParseEnvList splits an operator-supplied allow/deny list — comma, semicolon,
// or whitespace separated (e.g. the value of a CONTENOX_SANDBOX_ENV_ALLOW
// setting) — into entries, trimming blanks. It does not validate glob syntax; an
// unrecognized entry simply matches nothing, so a typo fails closed. Returns nil
// for an empty or all-blank input.
func ParseEnvList(s string) []string {
	fields := strings.FieldsFunc(s, func(r rune) bool {
		return r == ',' || r == ';' || unicode.IsSpace(r)
	})
	if len(fields) == 0 {
		return nil
	}
	return fields
}

// matchesAny reports whether name matches any pattern in patterns (see
// envNameMatches for the glob grammar).
func matchesAny(patterns []string, name string) bool {
	for _, pat := range patterns {
		if envNameMatches(pat, name) {
			return true
		}
	}
	return false
}

// envNameMatches reports whether an environment-variable name matches a pattern.
// The grammar is intentionally tiny — an exact name, or a name with a single
// leading OR trailing "*": "LC_*" is a prefix match, "*_TOKEN" a suffix match,
// "*" matches everything. Matching is case-sensitive, as environment variable
// names are on the platforms this runs on. An empty pattern matches nothing, so
// blank list entries are inert.
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

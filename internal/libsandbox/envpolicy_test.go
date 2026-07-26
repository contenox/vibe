package libsandbox

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// The headline: the default policy, resolved against a realistic parent
// environment, keeps the handful of safe operational variables and drops every
// credential — the whole point of the whitelist, with no per-consumer list to
// author.
func TestUnit_DefaultEnvPolicy_ResolveKeepsSafeBaseDropsSecrets(t *testing.T) {
	parent := []string{
		"PATH=/usr/bin",
		"LANG=en_US.UTF-8",
		"LC_ALL=C",
		"USER=agent",
		"HOME=/home/real", // present but never in the allow list — scrubEnv forces HOME
		"AWS_SECRET_ACCESS_KEY=shhh",
		"ANTHROPIC_API_KEY=sk-ant-xxx",
		"CONTENOX_TOKEN=leak",
		"GITHUB_TOKEN=ghp_xxx",
	}

	got := DefaultEnvPolicy().Resolve(parent)

	require.Equal(t, []string{"LANG", "LC_ALL", "PATH", "USER"}, got)
}

// Resolve returns only names that are actually set: an allowed name absent from
// the parent cannot be "passed through", so it never appears.
func TestUnit_EnvPolicy_ResolveOnlyReturnsNamesPresentInParent(t *testing.T) {
	got := DefaultEnvPolicy().Resolve([]string{"PATH=/usr/bin"})
	require.Equal(t, []string{"PATH"}, got)
}

// Deny wins over Allow: widening Allow to "AWS_*" to get the benign AWS_REGION
// must still not leak AWS_SECRET_ACCESS_KEY (default deny "*_KEY") or
// AWS_SESSION_TOKEN (default deny "*_TOKEN").
func TestUnit_EnvPolicy_DenyWinsOverAllow(t *testing.T) {
	parent := []string{
		"AWS_REGION=eu-west-1",
		"AWS_SECRET_ACCESS_KEY=shhh",
		"AWS_SESSION_TOKEN=tok",
	}

	got := DefaultEnvPolicy().Allowing("AWS_*").Resolve(parent)

	require.Equal(t, []string{"AWS_REGION"}, got)
}

// Glob rules: a trailing "*" is a prefix match (LC_*), a leading "*" a suffix
// match (*_PROXY). Both resolve to the concrete names present in the parent.
func TestUnit_EnvPolicy_GlobPrefixAndSuffix(t *testing.T) {
	parent := []string{
		"LC_ALL=C",
		"LC_CTYPE=UTF-8",
		"HTTP_PROXY=http://p",
		"HTTPS_PROXY=http://p",
		"NO_PROXY=localhost",
		"PATH=/usr/bin",
	}

	got := EnvPolicy{Allow: []string{"LC_*", "*_PROXY"}}.Resolve(parent)

	require.Equal(t, []string{"HTTPS_PROXY", "HTTP_PROXY", "LC_ALL", "LC_CTYPE", "NO_PROXY"}, got)
}

// The resolved list is de-duplicated and sorted, so the same environment always
// yields the same EnvAllow — a duplicate name in the parent collapses to one.
func TestUnit_EnvPolicy_ResolveSortedAndDeduped(t *testing.T) {
	parent := []string{"TERM=xterm", "PATH=/a", "PATH=/b"}

	got := DefaultEnvPolicy().Resolve(parent)

	require.Equal(t, []string{"PATH", "TERM"}, got)
}

// A parent entry with no "=" is not a KEY=VALUE variable; there is no name to
// match, so it is skipped rather than mishandled.
func TestUnit_EnvPolicy_ResolveSkipsMalformedParentEntry(t *testing.T) {
	got := DefaultEnvPolicy().Resolve([]string{"NOEQUALS", "PATH=/usr/bin"})
	require.Equal(t, []string{"PATH"}, got)
}

// A bare "*" is the explicit "trust this environment, just strip known secrets"
// mode: everything passes EXCEPT what Deny vetoes, which still removes the
// control plane and credential shapes.
func TestUnit_EnvPolicy_BareStarAllowsAllButDenyStillWins(t *testing.T) {
	parent := []string{
		"PATH=/usr/bin",
		"RANDOM_VAR=ok",
		"CONTENOX_DB=/x",
		"SOME_API_KEY=secret",
	}

	got := DefaultEnvPolicy().Allowing("*").Resolve(parent)

	require.Equal(t, []string{"PATH", "RANDOM_VAR"}, got)
}

// Allowing / Denying return copies: a caller's additions must never mutate the
// receiver or the slices DefaultEnvPolicy hands out (which are shared literals).
func TestUnit_EnvPolicy_AllowingDenyingReturnCopies(t *testing.T) {
	base := DefaultEnvPolicy()
	baseAllowLen := len(base.Allow)
	baseDenyLen := len(base.Deny)

	extended := base.Allowing("HTTP_PROXY").Denying("DANGER_*")

	require.Len(t, base.Allow, baseAllowLen, "base Allow must be unchanged")
	require.Len(t, base.Deny, baseDenyLen, "base Deny must be unchanged")
	require.Contains(t, extended.Allow, "HTTP_PROXY")
	require.Contains(t, extended.Deny, "DANGER_*")
	require.NotContains(t, base.Allow, "HTTP_PROXY")

	// A second DefaultEnvPolicy must not have been aliased/tainted.
	require.NotContains(t, DefaultEnvPolicy().Allow, "HTTP_PROXY")
}

// HOME is never a default passthrough: scrubEnv forces the scoped HOME, and
// inheriting the real one would defeat the mechanism that keeps ~/.ssh, ~/.aws,
// and ~/.contenox out of reach.
func TestUnit_DefaultEnvAllow_ExcludesHome(t *testing.T) {
	require.NotContains(t, DefaultEnvAllow(), "HOME")
}

// The control plane's own variables are always vetoed — this is the one thing
// that must never leak, regardless of how Allow is widened.
func TestUnit_DefaultEnvDeny_VetoesControlPlane(t *testing.T) {
	require.True(t, matchesAny(DefaultEnvDeny(), "CONTENOX_DB"))
	require.True(t, matchesAny(DefaultEnvDeny(), "CONTENOX_TOKEN"))
}

// The glob grammar in one table: exact, prefix ("X*"), suffix ("*X"), bare "*",
// the empty pattern (inert), non-matches, and case sensitivity.
func TestUnit_envNameMatches(t *testing.T) {
	cases := []struct {
		pattern, name string
		want          bool
	}{
		{"PATH", "PATH", true},
		{"PATH", "PATHEXT", false},
		{"LC_*", "LC_ALL", true},
		{"LC_*", "LC_", true},
		{"LC_*", "LANG", false},
		{"*_TOKEN", "GITHUB_TOKEN", true},
		{"*_TOKEN", "TOKEN_SUFFIX", false},
		{"*", "ANYTHING", true},
		{"", "PATH", false},
		{"path", "PATH", false}, // case-sensitive
	}
	for _, c := range cases {
		require.Equalf(t, c.want, envNameMatches(c.pattern, c.name),
			"envNameMatches(%q, %q)", c.pattern, c.name)
	}
}

// Operator input is split on commas, semicolons, and any whitespace, trimmed,
// with blanks dropped; empty input yields nil so "unset" is not "allow ”".
func TestUnit_ParseEnvList(t *testing.T) {
	require.Equal(t, []string{"PATH", "LC_*", "HTTP_PROXY", "GOCACHE"},
		ParseEnvList("PATH, LC_*  HTTP_PROXY;GOCACHE"))
	require.Equal(t, []string{"A", "B"}, ParseEnvList("A\nB\t"))
	require.Nil(t, ParseEnvList(""))
	require.Nil(t, ParseEnvList("   ,; \n "))
}

// End-to-end proof that the policy layer plugs into the existing mechanism: a
// resolved EnvPolicy fed to scrubEnv yields a confined environment with only the
// safe variables, the forced scoped HOME, and the emulated canonical PATH — no
// secret, no real home, and NOT the operator's /usr/bin PATH (that value is
// allow-copied by the policy but neutralized by scrubEnv's PATH emulation).
func TestUnit_EnvPolicy_ResolveFeedsScrubEnv(t *testing.T) {
	parent := []string{
		"PATH=/usr/bin",
		"LANG=C",
		"HOME=/home/real",
		"AWS_SECRET_ACCESS_KEY=shhh",
		"CONTENOX_TOKEN=leak",
	}

	allow := DefaultEnvPolicy().Resolve(parent)
	got := scrubEnv(parent, allow, nil, "/scoped/home")

	require.Equal(t, []string{
		"HOME=/scoped/home",
		"LANG=C",
		"PATH=" + canonicalPATH(),
	}, got)
}

// Apply returns the surviving KEY=VALUE entries (not names) for a native shell,
// and does NOT force a scoped HOME: the operator's real HOME passes through the
// policy like any other allowed variable, while credentials are still stripped.
func TestUnit_EnvPolicy_ApplyReturnsPairsAndDoesNotForceHome(t *testing.T) {
	parent := []string{
		"PATH=/usr/bin",
		"HOME=/home/real",
		"GITHUB_TOKEN=ghp_xxx",
		"CONTENOX_DB=/x",
	}

	// deny-secrets posture: pass everything except the control plane and secrets.
	got := EnvPolicy{Allow: []string{"*"}, Deny: DefaultEnvDeny()}.Apply(parent)

	require.Equal(t, []string{"HOME=/home/real", "PATH=/usr/bin"}, got)
}

// DefaultEnvDeny is exactly the union of the two component vetoes, and each holds
// what its name promises — the split is what lets the two postures differ.
func TestUnit_EnvDeny_ControlPlaneAndSecretSplit(t *testing.T) {
	require.Equal(t, []string{"CONTENOX_*"}, ControlPlaneEnvDeny())
	require.Contains(t, SecretEnvDeny(), "*_TOKEN")
	require.NotContains(t, SecretEnvDeny(), "CONTENOX_*")
	require.Equal(t, append(ControlPlaneEnvDeny(), SecretEnvDeny()...), DefaultEnvDeny())
}

// The re-permit design: a strict allowlist denies only the control plane (not the
// credential shapes), so an operator can hand a shell one trusted secret by
// naming it in Allow — while CONTENOX_* stays absent no matter what.
func TestUnit_EnvPolicy_StrictCanRepermitOneCredential(t *testing.T) {
	strict := EnvPolicy{Allow: DefaultEnvAllow(), Deny: ControlPlaneEnvDeny()}
	parent := []string{
		"PATH=/usr/bin",
		"NPM_TOKEN=npm_trusted",
		"AWS_SECRET_ACCESS_KEY=shhh",
		"CONTENOX_TOKEN=leak",
	}

	got := strict.Allowing("NPM_TOKEN").Apply(parent)

	// NPM_TOKEN is re-permitted; PATH is in the base; the un-named AWS secret and
	// the control-plane token are absent.
	require.Equal(t, []string{"NPM_TOKEN=npm_trusted", "PATH=/usr/bin"}, got)
}

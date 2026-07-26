package libsandbox

import (
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// Only names in allow survive, HOME is forced to the scoped dir, and PATH is
// forced to the emulated canonical value even though the parent's PATH was
// allow-copied — the operator's profile-built PATH never rides through. This is
// the whole credential-leak fix plus the PATH-emulation invariant in one assertion.
func TestUnit_scrubEnv_OnlyAllowedNamesPass(t *testing.T) {
	parent := []string{
		"PATH=/usr/bin",
		"TERM=xterm",
		"AWS_SECRET_ACCESS_KEY=shhh",
		"HOME=/home/real",
		"CONTENOX_TOKEN=leak",
	}

	got := scrubEnv(parent, []string{"PATH", "TERM"}, nil, "/scoped/home")

	require.Equal(t, []string{
		"HOME=/scoped/home",
		"PATH=" + canonicalPATH(),
		"TERM=xterm",
	}, got)
}

// A credential-shaped variable that is not in allow must not appear anywhere in
// the output — neither its name nor its value.
func TestUnit_scrubEnv_CredentialVarNotInAllowIsDropped(t *testing.T) {
	parent := []string{"AWS_SECRET_ACCESS_KEY=shhh", "PATH=/usr/bin"}

	got := scrubEnv(parent, []string{"PATH"}, nil, "/h")

	for _, kv := range got {
		require.NotContains(t, kv, "AWS_SECRET_ACCESS_KEY")
		require.NotContains(t, kv, "shhh")
	}
}

// HOME is authoritative: neither the inherited value (even when HOME is in
// allow) nor an explicit EnvSet HOME can override the scoped home.
func TestUnit_scrubEnv_HomeForcedOverInheritedAndSet(t *testing.T) {
	parent := []string{"HOME=/home/real"}

	got := scrubEnv(parent, []string{"HOME"}, map[string]string{"HOME": "/set/home"}, "/scoped/home")

	require.Contains(t, got, "HOME=/scoped/home")
	require.NotContains(t, got, "HOME=/home/real")
	require.NotContains(t, got, "HOME=/set/home")
}

// set overrides an allow-copied value and can add variables not present in the
// parent at all. PATH is not in set here, so it is forced to the emulated
// canonical value rather than the allow-copied parent PATH.
func TestUnit_scrubEnv_SetOverridesAllowCopiedAndAddsExtras(t *testing.T) {
	parent := []string{"FOO=parent", "PATH=/usr/bin"}

	got := scrubEnv(parent, []string{"FOO", "PATH"}, map[string]string{"FOO": "explicit", "EXTRA": "x"}, "/h")

	require.Contains(t, got, "FOO=explicit")
	require.NotContains(t, got, "FOO=parent")
	require.Contains(t, got, "EXTRA=x")
	require.Contains(t, got, "PATH="+canonicalPATH())
	require.NotContains(t, got, "PATH=/usr/bin")
}

// An explicit EnvSet PATH is the one sanctioned override: unlike an allow-copied
// PATH (which scrubEnv neutralizes), a value the caller sets in EnvSet survives,
// because it is an opt-in extension Command validates against the carve-outs.
func TestUnit_scrubEnv_EnvSetPathOverridesCanonical(t *testing.T) {
	parent := []string{"PATH=/usr/bin"}

	got := scrubEnv(parent, []string{"PATH"}, map[string]string{"PATH": "/opt/tools/bin"}, "/h")

	require.Contains(t, got, "PATH=/opt/tools/bin")
	require.NotContains(t, got, "PATH="+canonicalPATH())
}

// With nothing allowed and nothing set, the confined process still gets exactly
// two variables: the forced scoped HOME and the emulated canonical PATH. The
// parent's PATH and secret are both absent — PATH because it is emulated, not
// inherited.
func TestUnit_scrubEnv_EmptyAllowYieldsHomeAndCanonicalPath(t *testing.T) {
	parent := []string{"PATH=/usr/bin", "SECRET=x"}

	got := scrubEnv(parent, nil, nil, "/scoped")

	require.Equal(t, []string{"HOME=/scoped", "PATH=" + canonicalPATH()}, got)
}

// The output is sorted by name and stable across identical calls: a pure,
// deterministic function is what makes the emitted environment auditable.
func TestUnit_scrubEnv_DeterministicSorted(t *testing.T) {
	parent := []string{"BETA=2", "ALPHA=1", "GAMMA=3"}
	allow := []string{"BETA", "ALPHA", "GAMMA"}

	got := scrubEnv(parent, allow, nil, "/h")

	names := make([]string, len(got))
	for i, kv := range got {
		names[i] = kv[:strings.IndexByte(kv, '=')]
	}
	require.True(t, sort.StringsAreSorted(names), "env names must be sorted: %v", names)
	require.Equal(t, got, scrubEnv(parent, allow, nil, "/h"))
}

// A malformed parent entry (no "=") has no name to match and is dropped. PATH is
// the emulated canonical value, not the allow-copied parent one.
func TestUnit_scrubEnv_SkipsMalformedParentEntry(t *testing.T) {
	parent := []string{"NOTAKEYVALUE", "PATH=/usr/bin"}

	got := scrubEnv(parent, []string{"PATH"}, nil, "/h")

	require.Equal(t, []string{"HOME=/h", "PATH=" + canonicalPATH()}, got)
}
